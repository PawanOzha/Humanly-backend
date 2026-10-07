package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	// One round trip per query. The database is remote, and preparing
	// statements adds extra trips on every new connection.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(ping); err != nil {
		pool.Close()
		return nil, err
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() {
	p.pool.Close()
}

func (p *Postgres) migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(81422026)`); err != nil {
		return err
	}
	for _, stmt := range schema {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		picture TEXT NOT NULL DEFAULT '',
		password_hash TEXT,
		google_sub TEXT UNIQUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		token_hash TEXT PRIMARY KEY,
		user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		expires_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id)`,
	`CREATE TABLE IF NOT EXISTS subscriptions (
		user_id UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
		plan TEXT NOT NULL,
		period TEXT NOT NULL,
		allowance INTEGER NOT NULL,
		used INTEGER NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS drafts (
		id UUID PRIMARY KEY,
		user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		kind TEXT NOT NULL,
		words INTEGER NOT NULL,
		score DOUBLE PRECISION,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS drafts_user_idx ON drafts (user_id, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS guest_allowances (
		ip_hash TEXT PRIMARY KEY,
		used INTEGER NOT NULL DEFAULT 0,
		consented BOOLEAN NOT NULL DEFAULT false,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
}

func (p *Postgres) CreateEmailUser(ctx context.Context, name, email, passwordHash string, sub Subscription) (User, Subscription, error) {
	id, err := newID()
	if err != nil {
		return User{}, Subscription{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return User{}, Subscription{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO users (id, email, name, password_hash) VALUES ($1, $2, $3, $4)`, id, email, name, passwordHash)
	if err != nil {
		if isUnique(err) {
			return User{}, Subscription{}, ErrEmailTaken
		}
		return User{}, Subscription{}, err
	}
	if err := insertSub(ctx, tx, id, sub); err != nil {
		return User{}, Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, Subscription{}, err
	}
	return User{ID: id, Email: email, Name: name, Provider: "email"}, sub, nil
}

func (p *Postgres) IdentityByEmail(ctx context.Context, email string) (identity, Subscription, error) {
	var row identity
	var plan string
	var sub Subscription
	err := p.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.picture, COALESCE(u.password_hash, ''), COALESCE(u.google_sub, ''),
		       s.plan, s.used, s.allowance, s.period
		FROM users u
		JOIN subscriptions s ON s.user_id = u.id
		WHERE u.email = $1`, email).Scan(
		&row.User.ID, &row.User.Email, &row.User.Name, &row.User.Picture, &row.PasswordHash, &row.GoogleSub,
		&plan, &sub.Used, &sub.Allowance, &sub.Period,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity{}, Subscription{}, ErrNotFound
	}
	if err != nil {
		return identity{}, Subscription{}, err
	}
	row.User.Provider = providerOf(row.GoogleSub, row.PasswordHash)
	sub.Plan = PlanID(plan)
	return row, sub, nil
}

func (p *Postgres) UpsertGoogle(ctx context.Context, subID, email, name, picture string, fresh Subscription) (User, Subscription, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return User{}, Subscription{}, err
	}
	defer tx.Rollback(ctx)

	var id, existingGoogle, existingEmail string
	err = tx.QueryRow(ctx, `SELECT id, email, COALESCE(google_sub, '') FROM users WHERE google_sub = $1 FOR UPDATE`, subID).Scan(&id, &existingEmail, &existingGoogle)
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET name = $2, picture = $3 WHERE id = $1`, id, name, picture); err != nil {
			return User{}, Subscription{}, err
		}
		sub, err := lockSub(ctx, tx, id)
		if err != nil {
			return User{}, Subscription{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, Subscription{}, err
		}
		return User{ID: id, Email: existingEmail, Name: name, Picture: picture, Provider: "google"}, sub, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, Subscription{}, err
	}

	err = tx.QueryRow(ctx, `SELECT id, COALESCE(google_sub, '') FROM users WHERE email = $1 FOR UPDATE`, email).Scan(&id, &existingGoogle)
	if err == nil {
		if existingGoogle != "" && existingGoogle != subID {
			return User{}, Subscription{}, ErrGoogleUsed
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET google_sub = $2, name = $3, picture = $4 WHERE id = $1`, id, subID, name, picture); err != nil {
			return User{}, Subscription{}, err
		}
		sub, err := lockSub(ctx, tx, id)
		if err != nil {
			return User{}, Subscription{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, Subscription{}, err
		}
		return User{ID: id, Email: email, Name: name, Picture: picture, Provider: "google"}, sub, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, Subscription{}, err
	}

	id, err = newID()
	if err != nil {
		return User{}, Subscription{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO users (id, email, name, picture, google_sub) VALUES ($1, $2, $3, $4, $5)`, id, email, name, picture, subID); err != nil {
		if isUnique(err) {
			return User{}, Subscription{}, ErrEmailTaken
		}
		return User{}, Subscription{}, err
	}
	if err := insertSub(ctx, tx, id, fresh); err != nil {
		return User{}, Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, Subscription{}, err
	}
	return User{ID: id, Email: email, Name: name, Picture: picture, Provider: "google"}, fresh, nil
}

func (p *Postgres) SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, Subscription, error) {
	var user User
	var googleSub, passwordHash, plan string
	var sub Subscription
	err := p.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.picture, COALESCE(u.google_sub, ''), COALESCE(u.password_hash, ''),
		       s.plan, s.used, s.allowance, s.period
		FROM sessions sess
		JOIN users u ON u.id = sess.user_id
		JOIN subscriptions s ON s.user_id = u.id
		WHERE sess.token_hash = $1 AND sess.expires_at > $2`, tokenHash, now).Scan(
		&user.ID, &user.Email, &user.Name, &user.Picture, &googleSub, &passwordHash,
		&plan, &sub.Used, &sub.Allowance, &sub.Period,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Subscription{}, ErrNotFound
	}
	if err != nil {
		return User{}, Subscription{}, err
	}
	user.Provider = providerOf(googleSub, passwordHash)
	sub.Plan = PlanID(plan)
	// Read-only on purpose. A new month is written under the row lock in
	// WithSubscription; writing here could overwrite a run that is spending.
	return user, normalize(sub, now), nil
}

func (p *Postgres) SaveSession(ctx context.Context, tokenHash, userID string, expires time.Time) error {
	if _, err := p.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`, tokenHash, userID, expires); err != nil {
		return err
	}
	// Expired sessions are dead weight; clear them as new ones arrive.
	_, _ = p.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	return nil
}

func (p *Postgres) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (p *Postgres) UpdateSubscription(ctx context.Context, userID string, sub Subscription) error {
	tag, err := p.pool.Exec(ctx, `UPDATE subscriptions SET plan = $2, used = $3, allowance = $4, period = $5, updated_at = now() WHERE user_id = $1`, userID, string(sub.Plan), sub.Used, sub.Allowance, sub.Period)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) WithSubscription(ctx context.Context, userID string, fn func(Subscription) (Subscription, error)) (Subscription, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Subscription{}, err
	}
	defer tx.Rollback(ctx)
	sub, err := lockSub(ctx, tx, userID)
	if err != nil {
		return Subscription{}, err
	}
	next, err := fn(sub)
	if err != nil {
		return Subscription{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET plan = $2, used = $3, allowance = $4, period = $5, updated_at = now() WHERE user_id = $1`, userID, string(next.Plan), next.Used, next.Allowance, next.Period); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Subscription{}, err
	}
	return next, nil
}

func (p *Postgres) ListDrafts(ctx context.Context, userID string) ([]Draft, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, title, body, kind, words, score,
			(EXTRACT(EPOCH FROM created_at) * 1000)::bigint
		FROM drafts
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 12`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDrafts(rows)
}

func (p *Postgres) SaveDraft(ctx context.Context, userID string, draft Draft) ([]Draft, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1`, userID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// Saving the same text again replaces it instead of piling up copies.
	if _, err := tx.Exec(ctx, `DELETE FROM drafts WHERE user_id = $1 AND kind = $2 AND body = $3`, userID, draft.Kind, draft.Text); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO drafts (id, user_id, title, body, kind, words, score)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		draft.ID, userID, draft.Title, draft.Text, draft.Kind, draft.Words, draft.Score)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM drafts
		WHERE user_id = $1
		  AND id NOT IN (
			SELECT id FROM drafts WHERE user_id = $1 ORDER BY created_at DESC LIMIT 12
		  )`, userID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, title, body, kind, words, score,
			(EXTRACT(EPOCH FROM created_at) * 1000)::bigint
		FROM drafts
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 12`, userID)
	if err != nil {
		return nil, err
	}
	list, err := collectDrafts(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteDraft removes one document. The user_id match keeps a request from
// ever touching another account's rows.
func (p *Postgres) DeleteDraft(ctx context.Context, userID, id string) ([]Draft, error) {
	if !validUUID(id) {
		return nil, ErrNotFound
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM drafts WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return p.ListDrafts(ctx, userID)
}

func (p *Postgres) PeekGuest(ctx context.Context, key string, consent bool) (GuestPass, error) {
	var used int
	var consented bool
	err := p.pool.QueryRow(ctx, `SELECT used, consented FROM guest_allowances WHERE ip_hash = $1`, key).Scan(&used, &consented)
	if errors.Is(err, pgx.ErrNoRows) {
		return guestPass(0, consent), nil
	}
	if err != nil {
		return GuestPass{}, err
	}
	if consent && !consented {
		if _, err := p.pool.Exec(ctx, `UPDATE guest_allowances SET consented = true, updated_at = now() WHERE ip_hash = $1 AND NOT consented`, key); err != nil {
			return GuestPass{}, err
		}
		consented = true
	}
	return guestPass(used, consented), nil
}

func (p *Postgres) TakeGuest(ctx context.Context, key string, consent bool) (GuestPass, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return GuestPass{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO guest_allowances (ip_hash, used, consented)
		VALUES ($1, 0, $2)
		ON CONFLICT (ip_hash) DO UPDATE
		SET consented = guest_allowances.consented OR EXCLUDED.consented,
		    updated_at = now()`, key, consent); err != nil {
		return GuestPass{}, err
	}
	var used int
	var consented bool
	if err := tx.QueryRow(ctx, `SELECT used, consented FROM guest_allowances WHERE ip_hash = $1 FOR UPDATE`, key).Scan(&used, &consented); err != nil {
		return GuestPass{}, err
	}
	pass := guestPass(used, consented)
	if used >= pass.Allowance {
		if err := tx.Commit(ctx); err != nil {
			return GuestPass{}, err
		}
		return pass, ErrGuestLimit
	}
	used++
	if _, err := tx.Exec(ctx, `UPDATE guest_allowances SET used = $2, updated_at = now() WHERE ip_hash = $1`, key, used); err != nil {
		return GuestPass{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GuestPass{}, err
	}
	return guestPass(used, consented), nil
}

func (p *Postgres) RefundGuest(ctx context.Context, key string) error {
	_, err := p.pool.Exec(ctx, `UPDATE guest_allowances SET used = GREATEST(used - 1, 0), updated_at = now() WHERE ip_hash = $1`, key)
	return err
}

func collectDrafts(rows pgx.Rows) ([]Draft, error) {
	out := []Draft{}
	for rows.Next() {
		var draft Draft
		if err := rows.Scan(&draft.ID, &draft.Title, &draft.Text, &draft.Kind, &draft.Words, &draft.Score, &draft.At); err != nil {
			return nil, err
		}
		out = append(out, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertSub(ctx context.Context, tx execer, userID string, sub Subscription) error {
	_, err := tx.Exec(ctx, `INSERT INTO subscriptions (user_id, plan, period, allowance, used) VALUES ($1, $2, $3, $4, $5)`, userID, string(sub.Plan), sub.Period, sub.Allowance, sub.Used)
	return err
}

func lockSub(ctx context.Context, tx execer, userID string) (Subscription, error) {
	var sub Subscription
	var plan string
	err := tx.QueryRow(ctx, `SELECT plan, used, allowance, period FROM subscriptions WHERE user_id = $1 FOR UPDATE`, userID).Scan(&plan, &sub.Used, &sub.Allowance, &sub.Period)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, err
	}
	sub.Plan = PlanID(plan)
	return sub, nil
}

func providerOf(googleSub, passwordHash string) string {
	if googleSub != "" {
		return "google"
	}
	if passwordHash != "" {
		return "email"
	}
	return "email"
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
