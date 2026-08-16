// Command federation-gateway is the central sync and conflict-resolution
// service for federated IAG deployments.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	platformotel "github.com/alvor-technologies/iag-platform-go/otel"
	platformserviceauth "github.com/alvor-technologies/iag-platform-go/serviceauth"
	"github.com/jackc/pgx/v5/pgxpool"

	fgdb "github.com/iag/federation-gateway/db"
	"github.com/iag/federation-gateway/internal/config"
	"github.com/iag/federation-gateway/internal/db"
	"github.com/iag/federation-gateway/internal/events"
	"github.com/iag/federation-gateway/internal/handlers"
	"github.com/iag/federation-gateway/internal/middleware"
	"github.com/iag/federation-gateway/internal/migrate"
	"github.com/iag/federation-gateway/internal/models"
	"github.com/iag/federation-gateway/internal/outbox"
	"github.com/iag/federation-gateway/internal/platformauth"
	"github.com/iag/federation-gateway/internal/router"
	"github.com/iag/federation-gateway/internal/store"
)

func main() {
	configureLogger()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()

	tp, err := platformotel.Init(ctx, platformotel.Config{
		ServiceName: cfg.ServiceName,
		Environment: cfg.Environment,
	})
	if err != nil {
		slog.Warn("otel disabled", "err", err)
	} else {
		defer func() {
			shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = tp.Shutdown(shutdownCtx)
		}()
	}

	// --- persistence ---
	connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		slog.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if cfg.AutoMigrate {
		if err := autoMigrate(context.Background(), pool); err != nil {
			slog.Error("auto-migrate", "err", err)
			os.Exit(1)
		}
	} else {
		slog.Info("auto-migrate disabled — assuming schema is current")
	}

	st := store.New(pool)

	// --- inbound auth ---
	// A transient JWKS failure must not crash-loop the container. Boot degrades
	// to the background refresh (requests fail closed until keys load) and the
	// loop retries hard while the key set is empty.
	verifier := platformauth.NewVerifier(cfg.JWKSURL, cfg.JWTIssuer, cfg.Audience)
	bootstrapJWKS(verifier)
	go jwksRefreshLoop(ctx, verifier)

	if cfg.ServiceClientSecret != "" {
		go registerPermissionsLoop(ctx, cfg)
	} else {
		slog.Warn("SERVICE_CLIENT_SECRET unset — skipping permission registration")
	}

	// --- events + outbox ---
	eventBus := events.New(events.Config{Brokers: cfg.KafkaBrokers, Enabled: cfg.EventBusEnabled})
	defer func() { _ = eventBus.Close() }()
	outboxStore := outbox.NewStore(pool)
	if eventBus.Enabled() {
		eventBus.SetOutbox(outboxStore)
		go outbox.NewPublisher(outboxStore, eventBus).Run(ctx)
		slog.Info("event bus enabled", "brokers", cfg.KafkaBrokers, "outbox", true)
	}

	// --- HTTP ---
	api := &handlers.API{Cfg: cfg, Store: st, Events: eventBus, Outbox: outboxStore}
	platformAuth := middleware.NewPlatformAuth(verifier)
	engine := router.New(router.Options{Cfg: cfg, API: api, PlatformAuth: platformAuth})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       120 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		slog.Info("federation-gateway listening",
			"addr", cfg.Addr,
			"audience", cfg.Audience,
			"strategy", string(cfg.ConflictStrategy),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		slog.Info("shutdown", "signal", sig.String())
	case err := <-listenErr:
		slog.Error("listener died", "err", err)
		os.Exit(1)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
	cancelApp()
}

func configureLogger() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	var handler slog.Handler
	if strings.ToLower(os.Getenv("LOG_FORMAT")) == "json" {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))
}

func autoMigrate(parent context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	applied, err := migrate.Up(ctx, pool, fgdb.Migrations())
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) == 0 {
		slog.Info("schema already up to date")
	} else {
		slog.Info("migrations applied", "versions", applied)
	}
	return nil
}

// bootstrapJWKS fetches the key set with bounded retries so a cold auth service
// does not crash the container, then hands over to the background loop.
func bootstrapJWKS(v *platformauth.Verifier) {
	backoff := time.Second
	const (
		maxBackoff  = 15 * time.Second
		totalBudget = 2 * time.Minute
	)
	deadline := time.Now().Add(totalBudget)
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := v.Refresh(attemptCtx)
		cancel()
		if err == nil {
			slog.Info("jwks bootstrap ok", "attempt", attempt)
			return
		}
		if time.Now().After(deadline) {
			slog.Error("jwks bootstrap budget exhausted; continuing with empty key set", "attempts", attempt, "err", err)
			return
		}
		slog.Warn("jwks bootstrap failed; retrying", "attempt", attempt, "err", err, "backoff", backoff)
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// jwksRefreshLoop runs at two speeds: fast while the key set is empty (every
// request is failing closed, so recovery must be quick) and slow once loaded.
func jwksRefreshLoop(ctx context.Context, v *platformauth.Verifier) {
	const (
		steadyInterval   = 5 * time.Minute
		degradedInterval = 15 * time.Second
	)
	for {
		wait := steadyInterval
		if !v.HasKeys() {
			wait = degradedInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		hadKeys := v.HasKeys()
		refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := v.Refresh(refreshCtx)
		cancel()
		switch {
		case err != nil && hadKeys:
			slog.Warn("jwks refresh failed; serving with the previous key set", "err", err)
		case err != nil:
			slog.Error("jwks still unavailable; all authenticated requests are being rejected", "err", err)
		case !hadKeys:
			slog.Info("jwks recovered; token verification restored")
		}
	}
}

func registerPermissionsLoop(ctx context.Context, cfg config.Config) {
	saClient := platformserviceauth.NewClient(platformserviceauth.Options{
		TokenURL:     cfg.AuthTokenURL,
		ClientID:     cfg.ServiceClientID,
		ClientSecret: cfg.ServiceClientSecret,
		Audience:     "iag.authentication",
	})
	descriptors := models.PermissionDescriptors()
	perms := make([]platformserviceauth.Permission, 0, len(descriptors))
	for _, d := range descriptors {
		perms = append(perms, platformserviceauth.Permission{Name: d.Name, Description: d.Description})
	}
	backoff := time.Second
	const maxBackoff = 5 * time.Minute
	for {
		regCtx, c := context.WithTimeout(ctx, 10*time.Second)
		err := platformserviceauth.RegisterPermissions(regCtx, saClient, cfg.JWTIssuer, "federation-gateway", perms)
		c()
		if err == nil {
			slog.Info("permissions registered", "count", len(perms))
			return
		}
		// 401/403 is a credential misconfiguration that retrying cannot fix;
		// name the variables to check rather than looping quietly forever.
		if isClientAuthFailure(err) {
			slog.Error("permission registration rejected — check SERVICE_CLIENT_ID/SERVICE_CLIENT_SECRET against the auth service's SERVICE_CLIENT_SECRETS_JSON",
				"client_id", cfg.ServiceClientID, "token_url", cfg.AuthTokenURL, "err", err)
		} else {
			slog.Warn("permissions register failed", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func isClientAuthFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "token endpoint 401") ||
		strings.Contains(msg, "token endpoint 403") ||
		strings.Contains(msg, "register status 401") ||
		strings.Contains(msg, "register status 403")
}
