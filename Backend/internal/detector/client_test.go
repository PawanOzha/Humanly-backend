package detector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDetectMapsAPIScores(t *testing.T) {
	const src = "In today's landscape, it is important. Moreover, a curriculum is crucial."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/detect/text" {
			t.Fatalf("path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["threshold"].(float64) != 0.6 || body["batch_size"].(float64) != 16 {
			t.Fatalf("detect body = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"document_summary": map[string]any{
				"prediction":     "Inconclusive",
				"ai_probability": 0.8006277978420258,
			},
			"sentences": []map[string]any{
				{"sentence": "In today's landscape, it is important.", "prediction": "AI-generated", "ai_probability": 0.93},
				{"sentence": "Moreover, a curriculum is crucial.", "prediction": "Inconclusive", "ai_probability": 0.671},
			},
		})
	}))
	defer srv.Close()

	report, err := New(srv.URL).Detect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if report.Score != 80 || report.Label != "Inconclusive" || len(report.Sentences) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if report.Sentences[0].Score != 93 || report.Sentences[0].Note != "Feels AI" || !report.Sentences[0].Flagged {
		t.Fatalf("first = %+v", report.Sentences[0])
	}
	if report.Sentences[1].Score != 67 || report.Sentences[1].Note != "Leans AI" || report.Sentences[1].Flagged {
		t.Fatalf("second = %+v", report.Sentences[1])
	}
	joined := report.Sentences[0].Text + report.Sentences[1].Text
	if joined != src {
		t.Fatalf("joined = %q", joined)
	}
}

func TestHumanizeSendsTone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["p_syn"].(float64) != 0.06 || body["p_trans"].(float64) != 0 || body["preserve_linebreaks"] != true {
			t.Fatalf("settings = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"humanized_text": "A calmer line."})
	}))
	defer srv.Close()

	out, err := New(srv.URL).Humanize(context.Background(), "Moreover, we must leverage this.", "Academic")
	if err != nil {
		t.Fatal(err)
	}
	if out != "A calmer line." {
		t.Fatalf("out = %q", out)
	}
}

func TestToneWeights(t *testing.T) {
	var got struct {
		PSyn   float64 `json:"p_syn"`
		PTrans float64 `json:"p_trans"`
		Keep   bool    `json:"preserve_linebreaks"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"humanized_text": "Done."})
	}))
	defer srv.Close()
	c := New(srv.URL)
	want := []struct {
		mode   string
		pSyn   float64
		pTrans float64
	}{
		{"Academic", 0.06, 0},
		{"Balanced", 0.10, 0},
		{"Casual", 0.12, 0},
	}
	for _, tc := range want {
		if _, err := c.Humanize(context.Background(), "Hello there.", tc.mode); err != nil {
			t.Fatal(err)
		}
		if got.PSyn != tc.pSyn || got.PTrans != tc.pTrans || !got.Keep {
			t.Fatalf("%s sent %+v", tc.mode, got)
		}
	}
}

func TestOffline(t *testing.T) {
	c := New("http://127.0.0.1:1")
	c.HTTP.Timeout = 200 * time.Millisecond
	_, err := c.Detect(context.Background(), "Hello there.")
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("err = %v", err)
	}
}
