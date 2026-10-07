package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"humanly/backend/internal/account"
	"humanly/backend/internal/detector"
)

func TestSignupPlanAndQuota(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"humanized_text":   "A calmer line.",
			"document_summary": map[string]any{"prediction": "Human-written", "ai_probability": 0.1},
			"sentences":        []map[string]any{{"sentence": "A calmer line.", "prediction": "Human-written", "ai_probability": 0.1}},
		})
	}))
	t.Cleanup(up.Close)

	srv := httptest.NewServer((&Server{
		DevLocal: true,
		Pace:     -1,
		Accounts: account.New(account.NewMemory(), account.Config{SessionSecret: "test-session-secret-value"}),
		Detector: detector.New(up.URL),
		AppURL:   "http://localhost:3000",
	}).Handler())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	res := postJSON(t, client, srv.URL+"/v1/auth/signup", `{"name":"Ada Lovelace","email":"ada@example.com","password":"correct-horse"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("signup %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()

	me := getJSON(t, client, srv.URL+"/v1/auth/me")
	if me.StatusCode != http.StatusOK {
		t.Fatalf("me %d", me.StatusCode)
	}
	var session struct {
		Ready bool `json:"ready"`
		User  struct {
			Email string `json:"email"`
		} `json:"user"`
		Subscription struct {
			Plan string `json:"plan"`
			Used int    `json:"used"`
		} `json:"subscription"`
	}
	if err := json.NewDecoder(me.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if !session.Ready || session.User.Email != "ada@example.com" || session.Subscription.Plan != "free" {
		t.Fatalf("session = %+v", session)
	}

	res = postJSON(t, client, srv.URL+"/v1/billing/plan", `{"plan":"pro"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("plan %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()

	casual := `{"text":"A short academic line about the same point.","mode":"Casual"}`
	res = postJSON(t, client, srv.URL+"/v1/billing/plan", `{"plan":"free"}`)
	res.Body.Close()
	res = postJSON(t, client, srv.URL+"/v1/humanize", casual)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("tone %d %s", res.StatusCode, readBody(t, res))
	}
	var denied struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(res.Body).Decode(&denied)
	res.Body.Close()
	if denied.Code != "tone" {
		t.Fatalf("code = %q", denied.Code)
	}

	res = postJSON(t, client, srv.URL+"/v1/auth/logout", `{}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout %d", res.StatusCode)
	}
	res.Body.Close()
	me = getJSON(t, client, srv.URL+"/v1/auth/me")
	defer me.Body.Close()
	var after struct {
		User *struct{} `json:"user"`
	}
	if err := json.NewDecoder(me.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if after.User != nil {
		t.Fatal("session survived logout")
	}
}

func TestDraftsStayOnTheAccountThatSavedThem(t *testing.T) {
	srv := httptest.NewServer((&Server{
		DevLocal: true,
		Pace:     -1,
		Accounts: account.New(account.NewMemory(), account.Config{SessionSecret: "test-session-secret-value"}),
		AppURL:   "http://localhost:3000",
	}).Handler())
	t.Cleanup(srv.Close)

	ada := signedIn(t, srv.URL, `{"name":"Ada Lovelace","email":"ada@example.com","password":"correct-horse"}`)
	grace := signedIn(t, srv.URL, `{"name":"Grace Hopper","email":"grace@example.com","password":"correct-horse"}`)

	res := postJSON(t, ada, srv.URL+"/v1/drafts", `{"title":"Ada note","text":"Ada keeps this sentence.","kind":"write","score":12}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("ada save %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()

	res = postJSON(t, grace, srv.URL+"/v1/drafts", `{"title":"Grace note","text":"Grace keeps a different sentence.","kind":"pdf","score":40}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("grace save %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()

	adaDrafts := draftTitles(t, ada, srv.URL)
	graceDrafts := draftTitles(t, grace, srv.URL)
	if len(adaDrafts) != 1 || adaDrafts[0] != "Ada note" {
		t.Fatalf("ada drafts = %#v", adaDrafts)
	}
	if len(graceDrafts) != 1 || graceDrafts[0] != "Grace note" {
		t.Fatalf("grace drafts = %#v", graceDrafts)
	}

	anon, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	res = getJSON(t, &http.Client{Jar: anon}, srv.URL+"/v1/drafts")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon %d", res.StatusCode)
	}
	res.Body.Close()
}

func signedIn(t *testing.T, base, body string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	res := postJSON(t, client, base+"/v1/auth/signup", body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("signup %d %s", res.StatusCode, readBody(t, res))
	}
	res.Body.Close()
	return client
}

func draftTitles(t *testing.T, client *http.Client, base string) []string {
	t.Helper()
	res := getJSON(t, client, base+"/v1/drafts")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list %d %s", res.StatusCode, readBody(t, res))
	}
	defer res.Body.Close()
	var payload struct {
		Drafts []struct {
			Title string `json:"title"`
		} `json:"drafts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	titles := make([]string, len(payload.Drafts))
	for i, draft := range payload.Drafts {
		titles[i] = draft.Title
	}
	return titles
}

func postJSON(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	res, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getJSON(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	res, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
