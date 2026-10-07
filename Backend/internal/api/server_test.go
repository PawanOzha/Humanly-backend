package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"humanly/backend/internal/detector"
	"humanly/backend/internal/engine"
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/detect/text":
			pred := "Human-written"
			ai := 0.19
			if strings.Contains(body.Text, "Moreover") || strings.Contains(body.Text, "landscape") {
				pred = "AI-generated"
				ai = 0.93
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"document_summary": map[string]any{"prediction": pred, "ai_probability": ai},
				"sentences": []map[string]any{
					{"sentence": strings.TrimSpace(body.Text), "prediction": pred, "ai_probability": ai},
				},
			})
		case "/humanize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"humanized_text": "A calmer line about the same point.",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	s := &Server{DevLocal: true, Pace: -1, Detector: detector.New(up.URL)}
	return httptest.NewServer(s.Handler())
}

func TestDetectHTTP(t *testing.T) {
	srv := testServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/detect", "application/json", strings.NewReader(`{"text":"In today's fast-paced digital landscape, it is important to note that education must leverage robust technology."}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var report engine.Report
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Score != 93 || report.Label != "AI-generated" || len(report.Sentences) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Sentences[0].Note != "Feels AI" || report.Sentences[0].Score != 93 {
		t.Fatalf("sentence = %+v", report.Sentences[0])
	}
}

func TestLiveHumanize(t *testing.T) {
	srv := testServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ready outbound
	if err := json.Unmarshal(data, &ready); err != nil || ready.Op != "ready" {
		t.Fatalf("ready = %s", data)
	}

	req, _ := json.Marshal(inbound{Op: "humanize", ID: "job-1", Text: "Moreover, we cannot leverage this.", Mode: "Balanced"})
	if err := conn.Write(ctx, websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}

	var chunks int
	var result outbound
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var msg outbound
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.ID != "job-1" {
			t.Fatalf("id = %q", msg.ID)
		}
		switch msg.Op {
		case "chunk":
			chunks++
			if msg.Output == "" {
				t.Fatal("chunk missing output")
			}
		case "result":
			result = msg
		case "error":
			t.Fatalf("error %s", msg.Message)
		default:
			t.Fatalf("op %s", msg.Op)
		}
		if msg.Op == "result" {
			break
		}
	}
	if chunks == 0 {
		t.Fatal("no chunks streamed")
	}
	if result.Phase != "done" || result.Output != "A calmer line about the same point." {
		t.Fatalf("result = %+v", result)
	}
	if result.Before != 93 || result.Score != 19 || result.Label != "Human-written" || result.BeforeLabel != "AI-generated" {
		t.Fatalf("scores = %+v", result)
	}
}

func TestRejectsForeignOrigin(t *testing.T) {
	s := &Server{Origins: []string{"https://humanly.app"}, DevLocal: false, Pace: -1}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/detect", strings.NewReader(`{"text":"Hello there, this is a normal sentence."}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin was allowed")
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/detect", strings.NewReader(`{"text":"Hello there, this is a normal sentence."}`))
	req.Header.Set("Origin", "https://humanly.app")
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://humanly.app" {
		t.Fatalf("allow origin = %q", got)
	}
}
