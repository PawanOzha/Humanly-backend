package account

import (
	"context"
	"errors"
	"time"
)

var (
	ErrCredentials  = errors.New("Email or password is wrong.")
	ErrEmailTaken   = errors.New("An account already uses that email.")
	ErrGoogleLinked = errors.New("This account uses Google sign-in.")
	ErrGoogleUsed   = errors.New("That email is already linked to another Google account.")
	ErrNotFound     = errors.New("not found")
)

// InputError is a message that is safe to show the person who caused it.
type InputError string

func (e InputError) Error() string { return string(e) }

type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Picture  string `json:"picture,omitempty"`
	Provider string `json:"provider"`
}

type identity struct {
	User         User
	PasswordHash string
	GoogleSub    string
}

type Store interface {
	CreateEmailUser(ctx context.Context, name, email, passwordHash string, sub Subscription) (User, Subscription, error)
	IdentityByEmail(ctx context.Context, email string) (identity, Subscription, error)
	UpsertGoogle(ctx context.Context, subID, email, name, picture string, fresh Subscription) (User, Subscription, error)
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, Subscription, error)
	SaveSession(ctx context.Context, tokenHash, userID string, expires time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	UpdateSubscription(ctx context.Context, userID string, sub Subscription) error
	WithSubscription(ctx context.Context, userID string, fn func(Subscription) (Subscription, error)) (Subscription, error)
	ListDrafts(ctx context.Context, userID string) ([]Draft, error)
	SaveDraft(ctx context.Context, userID string, draft Draft) ([]Draft, error)
	DeleteDraft(ctx context.Context, userID, id string) ([]Draft, error)
	PeekGuest(ctx context.Context, key string, consent bool) (GuestPass, error)
	TakeGuest(ctx context.Context, key string, consent bool) (GuestPass, error)
	RefundGuest(ctx context.Context, key string) error
}
