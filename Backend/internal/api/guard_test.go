package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"humanly/backend/internal/account"
)

func guardServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&Server{
		DevLocal: true,
		Pace:     -1,
		Accounts: account.New(account.NewMemory(), account.Config{SessionSecret: "test-session-secret-value"}),
		AppURL:   "http://localhost:3000",
	}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestLimiterWindow(t *testing.T) {
	l := newLimiter(2, time.Minute)
	now := time.Now()
	if ok, _ := l.allow("a", now); !ok {
		t.Fatal("first hit refused")
	}
	if ok, _ := l.allow("a", now); !ok {
		t.Fatal("second hit refused")
	}
	if ok, wait := l.allow("a", now.Add(time.Second)); ok || wait <= 0 {
		t.Fatalf("third hit allowed=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("b", now); !ok {
		t.Fatal("other key was counted")
	}
	if ok, _ := l.allow("a", now.Add(time.Minute+time.Second)); !ok {
		t.Fatal("window did not reset")
	}
	var none *limiter
	if ok, _ := none.allow("a", now); !ok {
		t.Fatal("nil limiter must allow")
	}
}

func TestLoginIsRateLimitedPerEmail(t *testing.T) {
	srv := guardServer(t)
	client := &http.Client{}
	body := `{"email":"nobody@example.com","password":"not-the-password"}`
	var last int
	for i := 0; i < 12; i++ {
		res := postJSON(t, client, srv.URL+"/v1/auth/login", body)
		last = res.StatusCode
		res.Body.Close()
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("12th attempt = %d, want 429", last)
	}
}

func TestForeignOriginCannotWrite(t *testing.T) {
	srv := guardServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/logout", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin = %d, want 403", res.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/logout", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://localhost:3000")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("app origin = %d, want 204", res.StatusCode)
	}
}

func TestResponsesAreNotCached(t *testing.T) {
	srv := guardServer(t)
	res := getJSON(t, http.DefaultClient, srv.URL+"/v1/auth/me")
	res.Body.Close()
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
}

func TestDraftValidationAndDelete(t *testing.T) {
	srv := guardServer(t)
	client := signedIn(t, srv.URL, `{"name":"Ada Lovelace","email":"ada@example.com","password":"correct-horse"}`)

	res := postJSON(t, client, srv.URL+"/v1/drafts", `{"title":"Notes","text":"bad\u0000text","kind":"write"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("NUL text = %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()

	for i := 0; i < 2; i++ {
		res = postJSON(t, client, srv.URL+"/v1/drafts", `{"title":"Same","text":"Same words twice.","kind":"write"}`)
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("save = %d %s", res.StatusCode, readBody(t, res))
		}
		res.Body.Close()
	}
	res = postJSON(t, client, srv.URL+"/v1/drafts", `{"title":"Other","text":"A different note.","kind":"write"}`)
	var saved struct {
		Drafts []struct {
			ID string `json:"id"`
		} `json:"drafts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(saved.Drafts) != 2 {
		t.Fatalf("drafts = %d, want 2 (same text replaces itself)", len(saved.Drafts))
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/drafts/"+saved.Drafts[0].ID, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", res.StatusCode)
	}
	if got := len(draftTitles(t, client, srv.URL)); got != 1 {
		t.Fatalf("after delete = %d, want 1", got)
	}

	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/drafts/"+saved.Drafts[0].ID, nil)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", res.StatusCode)
	}
}

func TestDraftsBelongToOneAccount(t *testing.T) {
	srv := guardServer(t)
	ada := signedIn(t, srv.URL, `{"name":"Ada Lovelace","email":"ada@example.com","password":"correct-horse"}`)
	bob := signedIn(t, srv.URL, `{"name":"Bob Byron","email":"bob@example.com","password":"correct-horse"}`)

	res := postJSON(t, ada, srv.URL+"/v1/drafts", `{"title":"Private","text":"Only for Ada.","kind":"write"}`)
	var saved struct {
		Drafts []struct {
			ID string `json:"id"`
		} `json:"drafts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/drafts/"+saved.Drafts[0].ID, nil)
	res, err := bob.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("other account delete = %d, want 404", res.StatusCode)
	}
	if got := len(draftTitles(t, bob, srv.URL)); got != 0 {
		t.Fatalf("bob sees %d drafts", got)
	}
	if got := len(draftTitles(t, ada, srv.URL)); got != 1 {
		t.Fatalf("ada lost her draft: %d", got)
	}
}

func TestSignupRejectsControlCharacters(t *testing.T) {
	srv := guardServer(t)
	res := postJSON(t, http.DefaultClient, srv.URL+"/v1/auth/signup", `{"name":"Ada\u0000 Lovelace","email":"ada@example.com","password":"correct-horse"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("signup = %d", res.StatusCode)
	}
}
