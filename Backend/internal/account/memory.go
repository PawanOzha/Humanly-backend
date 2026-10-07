package account

import (
	"context"
	"sync"
	"time"
)

type Memory struct {
	mu       sync.Mutex
	users    map[string]identity
	byEmail  map[string]string
	byGoogle map[string]string
	sessions map[string]memSession
	subs     map[string]Subscription
	drafts   map[string][]Draft
	guests   map[string]guestRow
}

type guestRow struct {
	used      int
	consented bool
}

type memSession struct {
	UserID  string
	Expires time.Time
}

func NewMemory() *Memory {
	return &Memory{
		users:    map[string]identity{},
		byEmail:  map[string]string{},
		byGoogle: map[string]string{},
		sessions: map[string]memSession{},
		subs:     map[string]Subscription{},
		drafts:   map[string][]Draft{},
		guests:   map[string]guestRow{},
	}
}

func (m *Memory) CreateEmailUser(_ context.Context, name, email, passwordHash string, sub Subscription) (User, Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return User{}, Subscription{}, ErrEmailTaken
	}
	id, err := newID()
	if err != nil {
		return User{}, Subscription{}, err
	}
	user := User{ID: id, Email: email, Name: name, Provider: "email"}
	m.users[id] = identity{User: user, PasswordHash: passwordHash}
	m.byEmail[email] = id
	m.subs[id] = sub
	return user, sub, nil
}

func (m *Memory) IdentityByEmail(_ context.Context, email string) (identity, Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return identity{}, Subscription{}, ErrNotFound
	}
	return m.users[id], m.subs[id], nil
}

func (m *Memory) UpsertGoogle(_ context.Context, subID, email, name, picture string, fresh Subscription) (User, Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byGoogle[subID]; ok {
		row := m.users[id]
		row.User.Name = name
		row.User.Picture = picture
		row.User.Provider = "google"
		m.users[id] = row
		return row.User, m.subs[id], nil
	}
	if id, ok := m.byEmail[email]; ok {
		row := m.users[id]
		if row.GoogleSub != "" && row.GoogleSub != subID {
			return User{}, Subscription{}, ErrGoogleUsed
		}
		row.GoogleSub = subID
		row.User.Name = name
		row.User.Picture = picture
		row.User.Provider = "google"
		m.users[id] = row
		m.byGoogle[subID] = id
		return row.User, m.subs[id], nil
	}
	id, err := newID()
	if err != nil {
		return User{}, Subscription{}, err
	}
	user := User{ID: id, Email: email, Name: name, Picture: picture, Provider: "google"}
	m.users[id] = identity{User: user, GoogleSub: subID}
	m.byEmail[email] = id
	m.byGoogle[subID] = id
	m.subs[id] = fresh
	return user, fresh, nil
}

func (m *Memory) SessionUser(_ context.Context, tokenHash string, now time.Time) (User, Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[tokenHash]
	if !ok || !sess.Expires.After(now) {
		delete(m.sessions, tokenHash)
		return User{}, Subscription{}, ErrNotFound
	}
	row, ok := m.users[sess.UserID]
	if !ok {
		return User{}, Subscription{}, ErrNotFound
	}
	sub := normalize(m.subs[sess.UserID], now)
	m.subs[sess.UserID] = sub
	return row.User, sub, nil
}

func (m *Memory) SaveSession(_ context.Context, tokenHash, userID string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[tokenHash] = memSession{UserID: userID, Expires: expires}
	return nil
}

func (m *Memory) DeleteSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

func (m *Memory) UpdateSubscription(_ context.Context, userID string, sub Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[userID]; !ok {
		return ErrNotFound
	}
	m.subs[userID] = sub
	return nil
}

func (m *Memory) ListDrafts(_ context.Context, userID string) ([]Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyDrafts(m.drafts[userID]), nil
}

func (m *Memory) SaveDraft(_ context.Context, userID string, draft Draft) ([]Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[userID]; !ok {
		return nil, ErrNotFound
	}
	if draft.At == 0 {
		draft.At = time.Now().UnixMilli()
	}
	list := []Draft{draft}
	for _, old := range m.drafts[userID] {
		if old.Kind == draft.Kind && old.Text == draft.Text {
			continue
		}
		list = append(list, old)
	}
	if len(list) > maxDrafts {
		list = list[:maxDrafts]
	}
	m.drafts[userID] = list
	return copyDrafts(list), nil
}

func (m *Memory) DeleteDraft(_ context.Context, userID, id string) ([]Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.drafts[userID]
	next := make([]Draft, 0, len(list))
	for _, d := range list {
		if d.ID != id {
			next = append(next, d)
		}
	}
	if len(next) == len(list) {
		return nil, ErrNotFound
	}
	m.drafts[userID] = next
	return copyDrafts(next), nil
}

func copyDrafts(list []Draft) []Draft {
	if len(list) == 0 {
		return []Draft{}
	}
	out := make([]Draft, len(list))
	copy(out, list)
	return out
}

func (m *Memory) PeekGuest(_ context.Context, key string, consent bool) (GuestPass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.guests[key]
	if !ok {
		return guestPass(0, consent), nil
	}
	if consent {
		row.consented = true
		m.guests[key] = row
	}
	return guestPass(row.used, row.consented), nil
}

func (m *Memory) TakeGuest(_ context.Context, key string, consent bool) (GuestPass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.guests[key]
	if consent {
		row.consented = true
	}
	pass := guestPass(row.used, row.consented)
	if row.used >= pass.Allowance {
		m.guests[key] = row
		return pass, ErrGuestLimit
	}
	row.used++
	m.guests[key] = row
	return guestPass(row.used, row.consented), nil
}

func (m *Memory) RefundGuest(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.guests[key]
	if !ok || row.used < 1 {
		return nil
	}
	row.used--
	m.guests[key] = row
	return nil
}

func (m *Memory) WithSubscription(_ context.Context, userID string, fn func(Subscription) (Subscription, error)) (Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.subs[userID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	next, err := fn(sub)
	if err != nil {
		return Subscription{}, err
	}
	m.subs[userID] = next
	return next, nil
}
