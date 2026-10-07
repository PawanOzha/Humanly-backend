package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"humanly/backend/internal/account"
)

// limiter is a fixed-window counter keyed by caller. It is small on purpose:
// it only has to blunt password guessing and runaway scripts, and it keeps no
// state that outlives a window.
type limiter struct {
	mu    sync.Mutex
	max   int
	per   time.Duration
	rows  map[string]*window
	sweep time.Time
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(max int, per time.Duration) *limiter {
	return &limiter{max: max, per: per, rows: map[string]*window{}}
}

// allow counts one hit for key. When the window is full it reports how long
// the caller should wait.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.sweep) > l.per {
		for k, w := range l.rows {
			if now.Sub(w.start) >= l.per {
				delete(l.rows, k)
			}
		}
		l.sweep = now
	}
	w := l.rows[key]
	if w == nil || now.Sub(w.start) >= l.per {
		l.rows[key] = &window{start: now, n: 1}
		return true, 0
	}
	if w.n >= l.max {
		return false, l.per - now.Sub(w.start)
	}
	w.n++
	return true, 0
}

// tooMany answers a request that went past its limit.
func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{
		"error": "Too many attempts. Try again in a moment.",
		"code":  "rate",
	})
}

// secure sets the headers every API response should carry. Responses hold
// account data, so none of them may be stored by a shared cache.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", s.resourcePolicy(r))
		if s.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// sameSite refuses state-changing requests that name a foreign origin. The
// session cookie is SameSite=Lax already; this closes the remaining gap for
// sibling sites and odd browsers.
func (s *Server) sameSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin := r.Header.Get("Origin"); origin != "" && !s.allowOrigin(origin) {
				writeErr(w, http.StatusForbidden, "origin not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// appOrigin is the scheme and host the site is served from.
func (s *Server) appOrigin() string {
	u, err := url.Parse(s.appURL())
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// resourcePolicy lets the site read API responses when the API is on another host.
func (s *Server) resourcePolicy(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if origin != "" && s.allowOrigin(origin) && !strings.EqualFold(origin, s.appOrigin()) {
		return "cross-origin"
	}
	return "same-site"
}

// publicMessage is what a caller may read about an error. Anything that is
// not written for them is logged and replaced.
func (s *Server) publicMessage(err error) string {
	var in account.InputError
	if errors.As(err, &in) {
		return in.Error()
	}
	if errors.Is(err, account.ErrCredentials) || errors.Is(err, account.ErrEmailTaken) ||
		errors.Is(err, account.ErrGoogleLinked) || errors.Is(err, account.ErrGoogleUsed) {
		return err.Error()
	}
	s.warn("request failed", "err", err)
	return "Something went wrong on our side. Try again in a moment."
}

func (s *Server) trimmedEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
