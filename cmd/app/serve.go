package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/config"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/logging"
)

const shutdownTimeout = 20 * time.Second

// bootstrap loads config and opens the runtime database pool.
func bootstrap(ctx context.Context, stderr io.Writer, appName string) (config.Config, *slog.Logger, *pgxpool.Pool, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("config: %w", err)
	}
	log := logging.New(stderr, cfg.LogLevel).With("app", appName, "env", string(cfg.Env))
	pool, err := database.Open(ctx, cfg.DatabaseURL.Reveal(), database.Options{
		MaxConns:         cfg.DBMaxConns,
		StatementTimeout: 30 * time.Second,
		AppName:          appName,
	})
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	return cfg, log, pool, nil
}

func runAPI(ctx context.Context, stderr io.Writer) error {
	cfg, log, pool, err := bootstrap(ctx, stderr, "core-api")
	if err != nil {
		return err
	}
	defer pool.Close()
	core, err := app.NewCore(pool, log, clock.System{})
	if err != nil {
		return err
	}
	v1, err := core.NewAPI(cfg)
	if err != nil {
		return err
	}
	mux := healthMux(pool)
	mux.Handle("/v1/", v1)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		MaxHeaderBytes:    16 << 10,
	}
	return serve(ctx, log, srv)
}

func runWorker(ctx context.Context, stderr io.Writer) error {
	_, log, pool, err := bootstrap(ctx, stderr, "core-worker")
	if err != nil {
		return err
	}
	defer pool.Close()
	core, err := app.NewCore(pool, log, clock.System{})
	if err != nil {
		return err
	}
	w, err := core.NewWorker()
	if err != nil {
		return err
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("worker start: %w", err)
	}
	log.Info("worker started")
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := w.Stop(stopCtx); err != nil {
		return fmt.Errorf("worker stop: %w", err)
	}
	log.Info("worker stopped")
	return nil
}

// healthMux serves liveness and readiness probes. They expose no details
// beyond "ok"/"unavailable".
func healthMux(pool *pgxpool.Pool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

func serve(ctx context.Context, log *slog.Logger, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("http stopped")
	return nil
}
