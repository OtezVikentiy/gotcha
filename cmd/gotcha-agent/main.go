// cmd/gotcha-agent — тонкая точка входа: вся логика в internal/agent (сборка
// покрытия относит этот пакет к щадящей CMD-группе, не к BACK).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gitflic.ru/otezvikentiy/gotcha/internal/agent"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("gotcha-agent " + version.Version())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--check" {
		// Только валидация конфига, без сети и без цикла сбора — install.sh
		// зовёт это ДО systemctl enable, чтобы не соврать "installed and
		// running" на битом ключе/URL (ревью аудита ops-H2).
		if _, err := agent.LoadConfig(os.Getenv, os.Environ); err != nil {
			fmt.Fprintln(os.Stderr, "gotcha-agent --check: "+err.Error())
			os.Exit(2)
		}
		fmt.Println("gotcha-agent --check: config OK")
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := agent.LoadConfig(os.Getenv, os.Environ)
	if err != nil {
		logger.Error("config", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx, cfg, logger); err != nil {
		logger.Error("run", "error", err)
		os.Exit(1)
	}
}
