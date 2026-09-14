package db_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestApplySpanRetentionWarnsOnStaleRows: восстановление архива старше окна ретенции
// не должно проходить молча.
func TestApplySpanRetentionWarnsOnStaleRows(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const projectID = int64(9101)
	stale := time.Now().Add(-5 * 24 * time.Hour)
	if err := conn.Exec(ctx, "INSERT INTO spans (project_id, timestamp) VALUES (?, ?)", projectID, stale); err != nil {
		t.Fatalf("insert stale span: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := db.ApplySpanRetention(ctx, conn, 1); err != nil {
		t.Fatalf("ApplySpanRetention: %v", err)
	}
	// Восстанавливаем срок, который использует TestMigratePG и соседние тесты пакета —
	// иначе 1-дневный TTL остаётся на разделяемом контейнере дольше, чем нужно этому тесту.
	if err := db.ApplySpanRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplySpanRetention (restore): %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "table=spans") || !strings.Contains(out, "retention_days=1") {
		t.Fatalf("нет предупреждения о старых данных после восстановления архива старше окна ретенции:\n%s", out)
	}
}

// TestApplySpanRetentionSilentWhenNothingStale проверяет отрицательный случай: свежие
// данные внутри окна ретенции не должны вызывать предупреждение о старых данных.
func TestApplySpanRetentionSilentWhenNothingStale(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const projectID = int64(9102)
	fresh := time.Now().Add(-1 * time.Hour)
	if err := conn.Exec(ctx, "INSERT INTO spans (project_id, timestamp) VALUES (?, ?)", projectID, fresh); err != nil {
		t.Fatalf("insert fresh span: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := db.ApplySpanRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplySpanRetention: %v", err)
	}

	if strings.Contains(buf.String(), "table=spans") {
		t.Errorf("предупреждение о старых данных сработало без старых данных:\n%s", buf.String())
	}
}

// TestApplyTransactionRetentionWarnsOnStaleMVRows: предупреждение обязано срабатывать
// и на MV (applyMVTTL), не только на обычных таблицах (applyTableTTLColumn).
func TestApplyTransactionRetentionWarnsOnStaleMVRows(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const projectID = int64(9103)
	stale := time.Now().Add(-5 * 24 * time.Hour)
	// transactions_5m наполняется автоматически при вставке в transactions —
	// агрегатные состояния руками не собрать, а тут и не нужно.
	if err := conn.Exec(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, timestamp) VALUES (?, ?, ?, ?, ?)",
		projectID, "tr-stale", "sp-stale", "/stale", stale); err != nil {
		t.Fatalf("insert stale transaction: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := db.ApplyTransactionRetention(ctx, conn, 1); err != nil {
		t.Fatalf("ApplyTransactionRetention: %v", err)
	}
	// Восстанавливаем срок, которым пользуются соседние тесты пакета.
	if err := db.ApplyTransactionRetention(ctx, conn, 90); err != nil {
		t.Fatalf("ApplyTransactionRetention (restore): %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "table=transactions_5m") || !strings.Contains(out, "retention_days=1") {
		t.Fatalf("нет предупреждения о старых данных для MV transactions_5m:\n%s", out)
	}
}
