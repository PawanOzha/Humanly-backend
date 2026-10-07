package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	GuestWords        = 500
	guestTries        = 1
	guestConsentTries = 3
)

// GuestPass is how many free landing-page runs this network has left.
type GuestPass struct {
	Used      int `json:"used"`
	Allowance int `json:"allowance"`
	Left      int `json:"left"`
}

// ErrGuestLimit means the free checks on this network are used up.
var ErrGuestLimit = errors.New("Sign in to check or humanize another draft.")

// ErrGuestWords means an anonymous draft is over the free word cap.
var ErrGuestWords = errors.New("Sign in to check more than 500 words.")

func guestPass(used int, consent bool) GuestPass {
	allowance := guestTries
	if consent {
		allowance = guestConsentTries
	}
	left := allowance - used
	if left < 0 {
		left = 0
	}
	return GuestPass{Used: used, Allowance: allowance, Left: left}
}

func (s *Service) guestKey(ip string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte("guest:" + ip))
	return hex.EncodeToString(mac.Sum(nil))
}

// PeekGuest reports the free runs left for this network without spending one.
func (s *Service) PeekGuest(ctx context.Context, ip string, consent bool) (GuestPass, error) {
	return s.store.PeekGuest(ctx, s.guestKey(ip), consent)
}

// TakeGuest spends one free run for this network.
func (s *Service) TakeGuest(ctx context.Context, ip string, consent bool) (GuestPass, error) {
	return s.store.TakeGuest(ctx, s.guestKey(ip), consent)
}

// RefundGuest returns a free run when the check did not finish.
func (s *Service) RefundGuest(ctx context.Context, ip string) error {
	return s.store.RefundGuest(ctx, s.guestKey(ip))
}
