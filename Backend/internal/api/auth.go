package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"humanly/backend/internal/account"
	"humanly/backend/internal/engine"
)

const oauthCookie = "humanly_oauth"

type authBody struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type planBody struct {
	Plan string `json:"plan"`
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "Sign-in starts once the database is set on the server.")
		return
	}
	if !s.allowAuth(w, r, "") {
		return
	}
	var body authBody
	if !decodeJSON(w, r, &body) {
		return
	}
	user, sub, token, err := s.Accounts.Signup(r.Context(), body.Name, body.Email, body.Password)
	if err != nil {
		s.accountErr(w, err)
		return
	}
	s.setSession(w, token)
	s.writeAccount(w, r, http.StatusCreated, user, sub, false)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "Sign-in starts once the database is set on the server.")
		return
	}
	var body authBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if !s.allowAuth(w, r, body.Email) {
		return
	}
	user, sub, token, err := s.Accounts.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		s.accountErr(w, err)
		return
	}
	s.setSession(w, token)
	s.writeAccount(w, r, http.StatusOK, user, sub, true)
}

// allowAuth limits sign-in attempts per network and, for logins, per email
// address, so one account cannot be guessed at from many networks.
func (s *Server) allowAuth(w http.ResponseWriter, r *http.Request, email string) bool {
	now := time.Now()
	ok, wait := s.authLimit.allow(clientIP(r), now)
	if ok && email != "" {
		ok, wait = s.emailLimit.allow(s.trimmedEmail(email), now)
	}
	if !ok {
		tooMany(w, wait)
	}
	return ok
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.Accounts != nil {
		if c, err := r.Cookie(account.SessionCookie); err == nil {
			_ = s.Accounts.Logout(r.Context(), c.Value)
		}
	}
	s.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ready": false, "user": nil, "subscription": nil})
		return
	}
	cookie, err := r.Cookie(account.SessionCookie)
	if err != nil || cookie.Value == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "user": nil, "subscription": nil})
		return
	}
	user, sub, err := s.Accounts.FromToken(r.Context(), cookie.Value)
	if err != nil {
		s.clearSession(w)
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "user": nil, "subscription": nil})
		return
	}
	drafts, err := s.Accounts.Drafts(r.Context(), user.ID)
	if err != nil || drafts == nil {
		drafts = []account.Draft{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "user": user, "subscription": sub, "drafts": drafts})
}

func (s *Server) writeAccount(w http.ResponseWriter, r *http.Request, status int, user account.User, sub account.Subscription, withDrafts bool) {
	drafts := []account.Draft{}
	if withDrafts {
		if rows, err := s.Accounts.Drafts(r.Context(), user.ID); err == nil && rows != nil {
			drafts = rows
		}
	}
	writeJSON(w, status, map[string]any{"user": user, "subscription": sub, "drafts": drafts})
}

func (s *Server) setPlan(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "Plans connect once the database is set on the server.")
		return
	}
	user, _, ok := s.sessionUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Sign in to change plans.")
		return
	}
	var body planBody
	if !decodeJSON(w, r, &body) {
		return
	}
	sub, err := s.Accounts.SetPlan(r.Context(), user.ID, body.Plan)
	if err != nil {
		s.accountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

type draftBody struct {
	Title string   `json:"title"`
	Text  string   `json:"text"`
	Kind  string   `json:"kind"`
	Score *float64 `json:"score"`
}

func (s *Server) listDrafts(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"drafts": []account.Draft{}})
		return
	}
	user, _, ok := s.sessionUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Sign in to see your documents.")
		return
	}
	drafts, err := s.Accounts.Drafts(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Documents could not be loaded.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": drafts})
}

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "Documents are kept once the database is set on the server.")
		return
	}
	user, _, ok := s.sessionUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Sign in to save a document.")
		return
	}
	var body draftBody
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	drafts, err := s.Accounts.AddDraft(r.Context(), user.ID, body.Title, body.Text, body.Kind, body.Score)
	if err != nil {
		s.accountErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"drafts": drafts})
}

func (s *Server) deleteDraft(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "Documents are kept once the database is set on the server.")
		return
	}
	user, _, ok := s.sessionUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Sign in to manage documents.")
		return
	}
	drafts, err := s.Accounts.DeleteDraft(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.accountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": drafts})
}

func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	next := safePath(r.URL.Query().Get("next"))
	if s.Accounts == nil || !s.Accounts.GoogleReady() {
		http.Redirect(w, r, s.appURL()+"/login?error="+url.QueryEscape("Google sign-in is not configured yet."), http.StatusFound)
		return
	}
	state, err := s.Accounts.SignState(next)
	if err != nil {
		http.Redirect(w, r, s.appURL()+"/login?error="+url.QueryEscape("Google sign-in didn't complete."), http.StatusFound)
		return
	}
	http.SetCookie(w, s.cookie(oauthCookie, state, 600))
	http.Redirect(w, r, s.Accounts.GoogleAuthURL(state), http.StatusFound)
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, oauthCookie)
	fail := func(msg string) {
		http.Redirect(w, r, s.appURL()+"/login?error="+url.QueryEscape(msg), http.StatusFound)
	}
	if r.URL.Query().Get("error") != "" {
		fail("Google sign-in was cancelled.")
		return
	}
	if s.Accounts == nil || !s.Accounts.GoogleReady() {
		fail("Google sign-in is not configured yet.")
		return
	}
	cookie, err := r.Cookie(oauthCookie)
	state := r.URL.Query().Get("state")
	if err != nil || cookie.Value == "" || cookie.Value != state {
		fail("Google sign-in didn't complete.")
		return
	}
	next, err := s.Accounts.OpenState(state)
	if err != nil {
		fail("Google sign-in didn't complete.")
		return
	}
	_, _, token, err := s.Accounts.FinishGoogle(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		fail(s.publicMessage(err))
		return
	}
	s.setSession(w, token)
	http.Redirect(w, r, s.appURL()+safePath(next), http.StatusFound)
}

func (s *Server) chargeRewrite(r *http.Request, text, mode string) (string, *account.Subscription, error) {
	if s.Accounts == nil {
		return "", nil, nil
	}
	user, _, ok := s.sessionUser(r)
	if !ok {
		return "", nil, nil
	}
	sub, err := s.Accounts.Spend(r.Context(), user.ID, engine.WordCount(text), mode)
	if err != nil {
		return "", nil, err
	}
	return user.ID, &sub, nil
}

func (s *Server) sessionUser(r *http.Request) (account.User, account.Subscription, bool) {
	if s.Accounts == nil {
		return account.User{}, account.Subscription{}, false
	}
	c, err := r.Cookie(account.SessionCookie)
	if err != nil || c.Value == "" {
		return account.User{}, account.Subscription{}, false
	}
	user, sub, err := s.Accounts.FromToken(r.Context(), c.Value)
	if err != nil {
		return account.User{}, account.Subscription{}, false
	}
	return user, sub, true
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	s.setCookie(w, s.cookie(account.SessionCookie, token, int(account.SessionTTL.Seconds())))
}

func (s *Server) clearSession(w http.ResponseWriter) {
	s.clearCookie(w, account.SessionCookie)
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, s.cookie(name, "", -1))
}

// cookie builds a session cookie. When Google returns to a different host than
// the site, the cookie has to travel on that cross-site request.
func (s *Server) cookie(name, value string, maxAge int) *http.Cookie {
	secure, same := s.cookieSite()
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: same,
		MaxAge:   maxAge,
	}
}

func (s *Server) setCookie(w http.ResponseWriter, c *http.Cookie) {
	http.SetCookie(w, c)
}

func (s *Server) cookieSite() (secure bool, same http.SameSite) {
	secure = s.CookieSecure
	same = http.SameSiteLaxMode
	if s.Accounts == nil {
		return secure, same
	}
	raw := s.Accounts.GoogleRedirect()
	ru, err := url.Parse(raw)
	if err != nil || ru.Host == "" {
		return secure, same
	}
	au, err := url.Parse(s.appOrigin())
	if err != nil || au.Host == "" || strings.EqualFold(ru.Host, au.Host) {
		return secure, same
	}
	return true, http.SameSiteNoneMode
}

func (s *Server) appURL() string {
	if u := strings.TrimRight(strings.TrimSpace(s.AppURL), "/"); u != "" {
		return u
	}
	return "http://localhost:3000"
}

func safePath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "://") || strings.ContainsAny(raw, "\\\r\n") {
		return "/dashboard"
	}
	return raw
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<13))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func (s *Server) accountErr(w http.ResponseWriter, err error) {
	var lim *account.LimitError
	var in account.InputError
	switch {
	case errors.Is(err, account.ErrCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, account.ErrEmailTaken):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, account.ErrGoogleLinked), errors.Is(err, account.ErrGoogleUsed):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, account.ErrNotFound):
		writeErr(w, http.StatusNotFound, "That item is no longer there.")
	case errors.As(err, &lim):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": lim.Error(), "code": lim.Code})
	case errors.As(err, &in):
		writeErr(w, http.StatusBadRequest, in.Error())
	default:
		writeErr(w, http.StatusInternalServerError, s.publicMessage(err))
	}
}
