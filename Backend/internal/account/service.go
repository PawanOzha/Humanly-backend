package account

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookie = "humanly_session"
	SessionTTL    = 14 * 24 * time.Hour
	stateTTL      = 10 * time.Minute
)

var (
	emailRE   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	dummyHash []byte
)

func init() {
	var err error
	dummyHash, err = bcrypt.GenerateFromPassword([]byte("humanly-placeholder-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
}

type Config struct {
	SessionSecret      string
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string
}

type Service struct {
	store    Store
	secret   []byte
	googleID string
	googleS  string
	redirect string
	http     *http.Client
}

func New(store Store, cfg Config) *Service {
	return &Service{
		store:    store,
		secret:   []byte(cfg.SessionSecret),
		googleID: strings.TrimSpace(cfg.GoogleClientID),
		googleS:  strings.TrimSpace(cfg.GoogleClientSecret),
		redirect: strings.TrimRight(strings.TrimSpace(cfg.GoogleRedirectURL), "/"),
		http:     &http.Client{Timeout: 12 * time.Second},
	}
}

func (s *Service) GoogleReady() bool {
	return s != nil && s.googleID != "" && s.googleS != "" && s.redirect != ""
}

func (s *Service) Signup(ctx context.Context, name, email, password string) (User, Subscription, string, error) {
	name, email, err := cleanProfile(name, email)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	if err := cleanPassword(password); err != nil {
		return User{}, Subscription{}, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	user, sub, err := s.store.CreateEmailUser(ctx, name, email, string(hash), NewSubscription(PlanFree, time.Now()))
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	token, err := s.issue(ctx, user.ID)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	return user, sub, token, nil
}

func (s *Service) Login(ctx context.Context, email, password string) (User, Subscription, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	row, sub, err := s.store.IdentityByEmail(ctx, email)
	hash := dummyHash
	if err == nil && row.PasswordHash != "" {
		hash = []byte(row.PasswordHash)
	}
	match := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	if err != nil || !match {
		if err == nil && row.PasswordHash == "" && row.GoogleSub != "" {
			return User{}, Subscription{}, "", ErrGoogleLinked
		}
		return User{}, Subscription{}, "", ErrCredentials
	}
	sub, err = s.rollover(ctx, row.User.ID)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	token, err := s.issue(ctx, row.User.ID)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	return row.User, sub, token, nil
}

// rollover starts a new month under the subscription row lock, so a run that
// is spending words at the same moment cannot have its count overwritten.
func (s *Service) rollover(ctx context.Context, userID string) (Subscription, error) {
	return s.store.WithSubscription(ctx, userID, func(sub Subscription) (Subscription, error) {
		return normalize(sub, time.Now()), nil
	})
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(token))
}

func (s *Service) FromToken(ctx context.Context, token string) (User, Subscription, error) {
	if token == "" {
		return User{}, Subscription{}, ErrNotFound
	}
	return s.store.SessionUser(ctx, hashToken(token), time.Now())
}

func (s *Service) SetPlan(ctx context.Context, userID, plan string) (Subscription, error) {
	return s.store.WithSubscription(ctx, userID, func(sub Subscription) (Subscription, error) {
		return ApplyPlan(sub, PlanID(plan), time.Now())
	})
}

func (s *Service) Spend(ctx context.Context, userID string, words int, mode string) (Subscription, error) {
	return s.store.WithSubscription(ctx, userID, func(sub Subscription) (Subscription, error) {
		return ApplySpend(sub, words, mode, time.Now())
	})
}

func (s *Service) Refund(ctx context.Context, userID string, words int, period string) error {
	_, err := s.store.WithSubscription(ctx, userID, func(sub Subscription) (Subscription, error) {
		return ApplyRefund(sub, words, period), nil
	})
	return err
}

func (s *Service) GoogleRedirect() string {
	if s == nil {
		return ""
	}
	return s.redirect
}

func (s *Service) GoogleAuthURL(state string) string {
	q := url.Values{}
	q.Set("client_id", s.googleID)
	q.Set("redirect_uri", s.redirect)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("prompt", "select_account")
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
}

func (s *Service) FinishGoogle(ctx context.Context, code string) (User, Subscription, string, error) {
	profile, err := s.exchangeGoogle(ctx, code)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	name := strings.Join(strings.Fields(profile.Name), " ")
	if !validText(name, false) || utf8.RuneCountInString(name) < 2 || utf8.RuneCountInString(name) > 80 {
		name = strings.Split(profile.Email, "@")[0]
	}
	user, sub, err := s.store.UpsertGoogle(ctx, profile.Sub, profile.Email, name, profile.Picture, NewSubscription(PlanFree, time.Now()))
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	sub, err = s.rollover(ctx, user.ID)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	token, err := s.issue(ctx, user.ID)
	if err != nil {
		return User{}, Subscription{}, "", err
	}
	return user, sub, token, nil
}

type statePayload struct {
	Exp   int64  `json:"exp"`
	Next  string `json:"next"`
	Nonce string `json:"nonce"`
}

func (s *Service) SignState(next string) (string, error) {
	nonce, err := newToken()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(statePayload{Exp: time.Now().Add(stateTTL).Unix(), Next: next, Nonce: nonce})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) OpenState(value string) (string, error) {
	body, sig, ok := strings.Cut(value, ".")
	if !ok || body == "" || sig == "" {
		return "", errors.New("oauth state is invalid")
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", errors.New("oauth state is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", errors.New("oauth state is invalid")
	}
	var payload statePayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Exp < time.Now().Unix() {
		return "", errors.New("oauth state expired")
	}
	return payload.Next, nil
}

func (s *Service) issue(ctx context.Context, userID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.store.SaveSession(ctx, hashToken(token), userID, time.Now().Add(SessionTTL)); err != nil {
		return "", err
	}
	return token, nil
}

type googleProfile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func (s *Service) exchangeGoogle(ctx context.Context, code string) (googleProfile, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", s.googleID)
	form.Set("client_secret", s.googleS)
	form.Set("redirect_uri", s.redirect)
	form.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return googleProfile{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.http.Do(req)
	if err != nil {
		return googleProfile{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return googleProfile{}, InputError("Google sign-in didn't complete")
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		return googleProfile{}, InputError("Google sign-in didn't complete")
	}
	ureq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return googleProfile{}, err
	}
	ureq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	ures, err := s.http.Do(ureq)
	if err != nil {
		return googleProfile{}, err
	}
	defer ures.Body.Close()
	ubody, _ := io.ReadAll(io.LimitReader(ures.Body, 1<<20))
	if ures.StatusCode != http.StatusOK {
		return googleProfile{}, InputError("Couldn't read the Google account")
	}
	var profile googleProfile
	if err := json.Unmarshal(ubody, &profile); err != nil || profile.Sub == "" || profile.Email == "" {
		return googleProfile{}, InputError("That Google account has no email")
	}
	if !profile.EmailVerified {
		return googleProfile{}, InputError("That Google account email is not verified")
	}
	profile.Email = strings.ToLower(strings.TrimSpace(profile.Email))
	return profile, nil
}

func cleanProfile(name, email string) (string, string, error) {
	name = strings.Join(strings.Fields(name), " ")
	email = strings.ToLower(strings.TrimSpace(email))
	if !validText(name, false) || utf8.RuneCountInString(name) < 2 || utf8.RuneCountInString(name) > 80 {
		return "", "", InputError("Tell us what to call you.")
	}
	if !emailRE.MatchString(email) || len(email) > 254 {
		return "", "", InputError("Enter a valid email address.")
	}
	return name, email, nil
}

func cleanPassword(password string) error {
	if len(password) < 8 {
		return InputError("Use at least 8 characters.")
	}
	if len(password) > 72 {
		return InputError("Use at most 72 characters.")
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
