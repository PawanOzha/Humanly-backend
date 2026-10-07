package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"crypto/rand"
	"encoding/base64"

	"humanly/backend/internal/account"
	"humanly/backend/internal/api"
	"humanly/backend/internal/detector"
)

func main() {
	loadEnvFile()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := env("HUMANLY_ADDR", ":8080")
	upstream := env("HUMANLY_DETECTOR_URL", "http://127.0.0.1:8000")
	origins, devLocal := originsFromEnv()
	det := detector.New(upstream)
	appURL := env("HUMANLY_APP_URL", "http://localhost:3000")
	accounts := openAccounts(context.Background(), log, appURL)

	srv := &api.Server{
		Origins:      origins,
		DevLocal:     devLocal,
		Log:          log,
		Detector:     det,
		Accounts:     accounts,
		AppURL:       appURL,
		CookieSecure: env("HUMANLY_COOKIE_SECURE", "") == "true" || env("HUMANLY_COOKIE_SECURE", "") == "1",
	}
	go func() {
		if err := det.Warm(context.Background()); err != nil {
			log.Warn("detector warmup", "err", err, "upstream", upstream)
			return
		}
		log.Info("detector ready", "upstream", upstream)
	}()
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("humanly listening", "addr", addr, "dev_local", devLocal)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shut, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shut); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func openAccounts(ctx context.Context, log *slog.Logger, appURL string) *account.Service {
	url := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if url == "" {
		log.Info("accounts disabled", "reason", "set DATABASE_URL to enable sign-in, Google, and word credits")
		return nil
	}
	db, err := account.OpenPostgres(ctx, url)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	secret := strings.TrimSpace(os.Getenv("SESSION_SECRET"))
	if secret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			log.Error("session secret", "err", err)
			os.Exit(1)
		}
		secret = base64.RawURLEncoding.EncodeToString(buf)
		log.Warn("SESSION_SECRET is empty; sessions reset when the server restarts")
	}
	redirect := env("GOOGLE_REDIRECT_URL", strings.TrimRight(appURL, "/")+"/api/v1/auth/google/callback")
	log.Info("accounts ready", "google", strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")) != "")
	return account.New(db, account.Config{
		SessionSecret:      secret,
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:  redirect,
	})
}

func originsFromEnv() ([]string, bool) {
	raw := strings.TrimSpace(os.Getenv("HUMANLY_ORIGINS"))
	if raw == "" {
		return nil, true
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out, false
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// loadEnvFile fills unset variables from .env in the working directory.
// Values already present in the process environment win, so deploys can
// inject secrets without a file.
func loadEnvFile() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			q := val[0]
			if (q == '"' || q == '\'') && val[len(val)-1] == q {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, val)
	}
}
