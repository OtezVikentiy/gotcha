package main

import (
	"log/slog"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
)

func TestNewRootMuxNotesUnauthenticatedServiceEndpoints(t *testing.T) {
	prev := slog.Default()
	var records []slog.Record
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prev)

	var metrics selfmetrics.Registry
	newRootMux(rootDeps{selfMetrics: &metrics})

	var found bool
	for _, r := range records {
		if r.Level == slog.LevelInfo && strings.Contains(r.Message, "/metrics") &&
			strings.Contains(r.Message, "authentication") {
			found = true
		}
	}
	if !found {
		t.Fatal("newRootMux: нет напоминания об открытых без аутентификации служебных ручках")
	}
}
