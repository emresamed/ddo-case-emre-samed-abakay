// Command api runs the SahneBilet HTTP API.
//
//	api            migrate, seed if empty, serve
//	api reset-demo drop everything, migrate and seed again
//	api healthcheck  exit 0 if GET /health answers 200 (for Docker)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"sahnebilet/internal/config"
	"sahnebilet/internal/db"
	"sahnebilet/internal/httpapi"
	"sahnebilet/internal/seed"
	"sahnebilet/internal/service"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(cmd, log); err != nil {
		log.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + addr + "/health")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(cmd string, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, err := service.New(pool, cfg)
	if err != nil {
		return err
	}

	switch cmd {
	case "reset-demo":
		if err := db.Reset(ctx, pool); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		if err := seed.Run(ctx, pool, svc, cfg.SeedValue, cfg.ScheduleDays, seed.Today(cfg.TimeZone)); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		log.Info("demo data reset", "seed", cfg.SeedValue)
		return nil
	case "serve":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	if err := db.Migrate(pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	empty, err := seed.IsEmpty(ctx, pool)
	if err != nil {
		return err
	}
	if empty {
		start := time.Now()
		if err := seed.Run(ctx, pool, svc, cfg.SeedValue, cfg.ScheduleDays, seed.Today(cfg.TimeZone)); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		log.Info("database seeded", "seed", cfg.SeedValue, "took_ms", time.Since(start).Milliseconds())
	}

	go svc.RunSweeper(ctx, 15*time.Second, func(err error) { log.Error("hold sweeper", "error", err.Error()) })

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(svc, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			return err
		}
	}
	log.Info("stopped")
	return nil
}
