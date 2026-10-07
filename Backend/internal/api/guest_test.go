package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"humanly/backend/internal/account"
	"humanly/backend/internal/detector"
)

func TestGuestChecksStayOnTheCallingNetwork(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"document_summary": map[string]any{"prediction": "Human-written", "ai_probability": 0.1},
			"sentences":        []map[string]any{{"sentence": "A short anonymous line.", "prediction": "Human-written", "ai_probability": 0.1}},
		})
	}))
	t.Cleanup(up.Close)

	srv := httptest.NewServer((&Server{
		DevLocal: true,
		Pace:     -1,
		Detector: detector.New(up.URL),
		Accounts: account.New(account.NewMemory(), account.Config{SessionSecret: "test-session-secret-value"}),
	}).Handler())
	t.Cleanup(srv.Close)

	first := guestPost(t, srv.URL, "203.0.113.20", false)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first %d %s", first.StatusCode, readBody(t, first))
	}
	first.Body.Close()
	second := guestPost(t, srv.URL, "203.0.113.20", false)
	if second.StatusCode != http.StatusForbidden {
		t.Fatalf("second %d %s", second.StatusCode, readBody(t, second))
	}
	body := readBody(t, second)
	second.Body.Close()
	if !strings.Contains(body, `"code":"guest"`) {
		t.Fatalf("body = %s", body)
	}

	other := guestGet(t, srv.URL, "203.0.113.21", true)
	if other.StatusCode != http.StatusOK {
		t.Fatalf("other %d %s", other.StatusCode, readBody(t, other))
	}
	got := readBody(t, other)
	other.Body.Close()
	if !strings.Contains(got, `"allowance":3`) || !strings.Contains(got, `"left":3`) {
		t.Fatalf("peek = %s", got)
	}
}

func guestPost(t *testing.T, base, ip string, consent bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/detect", strings.NewReader(`{"text":"A short anonymous line."}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func guestGet(t *testing.T, base, ip string, consent bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", ip)
	if consent {
		req.AddCookie(&http.Cookie{Name: "humanly_consent", Value: "accepted"})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
