package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"humanly/backend/internal/account"
	"humanly/backend/internal/engine"
)

const consentCookie = "humanly_consent"

func (s *Server) guestStatus(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"signedIn": false, "used": 0, "allowance": 1, "left": 1})
		return
	}
	if _, _, ok := s.sessionUser(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"signedIn": true})
		return
	}
	pass, err := s.Accounts.PeekGuest(r.Context(), clientIP(r), consentCookieSet(r))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not read the free checks.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"signedIn":  false,
		"used":      pass.Used,
		"allowance": pass.Allowance,
		"left":      pass.Left,
	})
}

// beginGuest spends one anonymous run. Signed-in requests are not counted.
// The returned function gives the run back when the check does not finish.
func (s *Server) beginGuest(ctx context.Context, ip string, signedIn bool, text string, consent bool) (*account.GuestPass, func(), error) {
	noop := func() {}
	if signedIn || s.Accounts == nil {
		return nil, noop, nil
	}
	if engine.WordCount(text) > account.GuestWords {
		return nil, noop, account.ErrGuestWords
	}
	pass, err := s.Accounts.TakeGuest(ctx, ip, consent)
	if err != nil {
		return &pass, noop, err
	}
	spent := true
	refund := func() {
		if !spent {
			return
		}
		spent = false
		_ = s.Accounts.RefundGuest(context.Background(), ip)
	}
	return &pass, refund, nil
}

func (s *Server) guestErr(w http.ResponseWriter, err error, pass *account.GuestPass) {
	if !isGuestLimit(err) {
		writeErr(w, http.StatusServiceUnavailable, s.publicMessage(err))
		return
	}
	code := "guest"
	if errors.Is(err, account.ErrGuestWords) {
		code = "words"
	}
	body := map[string]any{"error": err.Error(), "code": code}
	if pass != nil {
		body["guest"] = pass
	}
	writeJSON(w, http.StatusForbidden, body)
}

func isGuestLimit(err error) bool {
	return errors.Is(err, account.ErrGuestLimit) || errors.Is(err, account.ErrGuestWords)
}

func consentCookieSet(r *http.Request) bool {
	c, err := r.Cookie(consentCookie)
	return err == nil && c.Value == "accepted"
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if fwd := forwardedIP(r); fwd != "" {
			return fwd
		}
	}
	if host == "" {
		return "unknown"
	}
	return host
}

func forwardedIP(r *http.Request) string {
	for _, header := range []string{"X-Forwarded-For", "X-Real-Ip"} {
		raw := strings.TrimSpace(r.Header.Get(header))
		if raw == "" {
			continue
		}
		first := strings.TrimSpace(strings.Split(raw, ",")[0])
		if net.ParseIP(first) != nil {
			return first
		}
	}
	return ""
}
