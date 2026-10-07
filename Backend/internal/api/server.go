// Package api is the Humanly HTTP and WebSocket server.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"humanly/backend/internal/account"
	"humanly/backend/internal/engine"
)

// Detector scores and rewrites a draft through the detection service.
type Detector interface {
	Detect(ctx context.Context, text string) (engine.Report, error)
	Humanize(ctx context.Context, text, mode string) (string, error)
}

const maxBody = 48 << 10

// Server is the production API the Humanly editor talks to.
type Server struct {
	Origins      []string
	DevLocal     bool
	Pace         time.Duration
	Log          *slog.Logger
	Detector     Detector
	Accounts     *account.Service
	AppURL       string
	CookieSecure bool

	authLimit  *limiter
	emailLimit *limiter
	jobLimit   *limiter
}

func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.authLimit = newLimiter(20, time.Minute)
	s.emailLimit = newLimiter(10, 15*time.Minute)
	s.jobLimit = newLimiter(40, time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/auth/signup", s.signup)
	mux.HandleFunc("POST /v1/auth/login", s.login)
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
	mux.HandleFunc("GET /v1/auth/me", s.me)
	mux.HandleFunc("GET /v1/auth/google", s.googleStart)
	mux.HandleFunc("GET /v1/auth/google/callback", s.googleCallback)
	// The site proxies this API under /api. Google must be able to call either path.
	mux.HandleFunc("GET /api/v1/auth/google", s.googleStart)
	mux.HandleFunc("GET /api/v1/auth/google/callback", s.googleCallback)
	mux.HandleFunc("POST /v1/billing/plan", s.setPlan)
	mux.HandleFunc("GET /v1/drafts", s.listDrafts)
	mux.HandleFunc("POST /v1/drafts", s.createDraft)
	mux.HandleFunc("DELETE /v1/drafts/{id}", s.deleteDraft)
	mux.HandleFunc("GET /v1/guest", s.guestStatus)
	mux.HandleFunc("POST /v1/detect", s.detect)
	mux.HandleFunc("POST /v1/humanize", s.humanize)
	mux.HandleFunc("GET /v1/live", s.live)
	return s.log(s.secure(s.cors(s.sameSite(mux))))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type textReq struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
}

func (s *Server) detect(w http.ResponseWriter, r *http.Request) {
	text, ok := s.readText(w, r)
	if !ok {
		return
	}
	user, _, signedIn := s.sessionUser(r)
	if !s.allowJob(w, user.ID, r) {
		return
	}
	pass, refund, err := s.beginGuest(r.Context(), clientIP(r), signedIn, text, consentCookieSet(r))
	if err != nil {
		s.guestErr(w, err, pass)
		return
	}
	defer func() { refund() }()
	report, err := s.score(r.Context(), text)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	refund = func() {}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) humanize(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	text, err := engine.CheckText(body.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !engine.ValidMode(body.Mode) {
		writeErr(w, http.StatusBadRequest, "unknown tone")
		return
	}
	member, _, _ := s.sessionUser(r)
	if !s.allowJob(w, member.ID, r) {
		return
	}
	userID, usage, err := s.chargeRewrite(r, text, body.Mode)
	if err != nil {
		s.accountErr(w, err)
		return
	}
	pass, refundGuest, err := s.beginGuest(r.Context(), clientIP(r), userID != "", text, consentCookieSet(r))
	if err != nil {
		s.guestErr(w, err, pass)
		return
	}
	defer func() { refundGuest() }()
	before, out, after, err := s.rewrite(r.Context(), text, body.Mode)
	if err != nil {
		if usage != nil {
			_ = s.Accounts.Refund(r.Context(), userID, engine.WordCount(text), usage.Period)
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	refundGuest = func() {}
	payload := map[string]any{
		"output":      out,
		"score":       after.Score,
		"before":      before.Score,
		"label":       after.Label,
		"beforeLabel": before.Label,
		"sentences":   after.Sentences,
		"original":    before.Sentences,
	}
	if usage != nil {
		payload["usage"] = usage
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) score(ctx context.Context, text string) (engine.Report, error) {
	if s.Detector == nil {
		return engine.Report{}, errOffline
	}
	report, err := s.Detector.Detect(ctx, text)
	if err != nil {
		s.warn("detect", "err", err)
		return engine.Report{}, err
	}
	return report, nil
}

func (s *Server) rewrite(ctx context.Context, text, mode string) (before engine.Report, out string, after engine.Report, err error) {
	if s.Detector == nil {
		return engine.Report{}, "", engine.Report{}, errOffline
	}
	before, err = s.Detector.Detect(ctx, text)
	if err != nil {
		s.warn("detect", "err", err)
		return engine.Report{}, "", engine.Report{}, err
	}
	out, err = s.Detector.Humanize(ctx, text, mode)
	if err != nil {
		s.warn("humanize", "err", err)
		return engine.Report{}, "", engine.Report{}, err
	}
	after, err = s.Detector.Detect(ctx, out)
	if err != nil {
		s.warn("detect", "err", err)
		return engine.Report{}, "", engine.Report{}, err
	}
	return before, out, after, nil
}

func (s *Server) warn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}

// allowJob spends one run from the caller's per-minute budget. Signed-in
// callers are counted by account, everyone else by network.
func (s *Server) allowJob(w http.ResponseWriter, userID string, r *http.Request) bool {
	ok, wait := s.jobLimit.allow(jobKey(userID, clientIP(r)), time.Now())
	if !ok {
		tooMany(w, wait)
	}
	return ok
}

func jobKey(userID, ip string) string {
	if userID != "" {
		return "u:" + userID
	}
	return "ip:" + ip
}

func (s *Server) readText(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, ok := s.readBody(w, r)
	if !ok {
		return "", false
	}
	text, err := engine.CheckText(body.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return text, true
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) (textReq, bool) {
	var body textReq
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return body, false
	}
	return body, true
}

func (s *Server) pace() time.Duration {
	if s.Pace < 0 {
		return 0
	}
	if s.Pace == 0 {
		return 42 * time.Millisecond
	}
	return s.Pace
}

func (s *Server) sleep(ctx context.Context) error {
	d := s.pace()
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Server) allowOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	if own := s.appOrigin(); own != "" && strings.EqualFold(own, origin) {
		return true
	}
	for _, allowed := range s.Origins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	if !s.DevLocal {
		return false
	}
	host := origin
	if i := strings.Index(origin, "://"); i >= 0 {
		host = origin[i+3:]
	}
	host = strings.Split(host, "/")[0]
	name := strings.Split(host, ":")[0]
	return name == "localhost" || name == "127.0.0.1"
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.allowOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &codeWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rw, r)
		if r.URL.Path == "/v1/live" {
			return
		}
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rw.code, "ms", time.Since(start).Milliseconds())
	})
}

type codeWriter struct {
	http.ResponseWriter
	code int
}

func (w *codeWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *codeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *codeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("websocket upgrade is not supported")
	}
	return h.Hijack()
}

func (w *codeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

var errOffline = strErr("detector is offline")

type strErr string

func (e strErr) Error() string { return string(e) }

func isClosed(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}
