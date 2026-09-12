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
		// валидация без сети и цикла сбора: install.sh вызывает это до systemctl enable
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
