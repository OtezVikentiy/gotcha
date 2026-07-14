package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gotcha failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv, os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.SecretKey == "insecure-dev-secret" {
		slog.Warn("GOTCHA_SECRET_KEY is not set, using insecure dev default")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := db.NewPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pg.Close()

	ch, err := db.NewClickHouse(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return err
	}
	defer ch.Close()

	// Сигнал во время миграций не прерывает их (golang-migrate не берёт
	// context) — процесс завершится после текущего шага.
	slog.Info("applying migrations")
	err = db.WithMigrationLock(ctx, pg, func() error {
		if err := db.MigratePG(cfg.PostgresDSN); err != nil {
			return err
		}
		if err := db.MigrateCH(cfg.ClickHouseDSN); err != nil {
			return err
		}
		return db.ApplyRetention(ctx, ch, cfg.RetentionDays)
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(pg, ch))

	// Общие сервисы нужны и ingest-у, и web-у — строим один раз на любой
	// активный режим, а не дублируем на каждый.
	var orgSvc *org.Service
	var issueSvc *issue.Service
	if cfg.Mode == "ingest" || cfg.Mode == "web" || cfg.Mode == "all" {
		orgSvc = org.NewService(pg, cfg.DefaultEventQuota)
		issueSvc = issue.NewService(pg)
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		batcher = event.NewBatcher(ch)
		go batcher.Run()
		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.Start()
		ingest.NewHandler(ingest.NewKeyCache(orgSvc), pipeline, cfg.MaxEventBytes).Register(mux)
		slog.Info("ingest enabled")
	}
	if cfg.Mode == "web" || cfg.Mode == "all" {
		authSvc := auth.NewService(pg)
		eventQuery := event.NewQuery(ch)
		web.New(authSvc, orgSvc, issueSvc, eventQuery, cfg.BaseURL).Register(mux)
		slog.Info("web enabled")
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "mode", cfg.Mode)
		errCh <- srv.ListenAndServe()
	}()

	drain := func() {
		if pipeline != nil {
			pipeline.Close()
		}
		if batcher != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := batcher.Close(cctx); err != nil {
				slog.Error("event batcher drain failed", "error", err)
			}
		}
	}

	select {
	case err := <-errCh:
		drain()
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			drain()
			return err
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			drain()
			return err
		}
		drain()
		return nil
	}
}
