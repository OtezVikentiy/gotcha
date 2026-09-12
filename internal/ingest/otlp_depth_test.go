package ingest_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestOTLPJSONRejectsDeepBody(t *testing.T) {
	s := newStack(t)
	body := []byte(strings.Repeat("[", 5000) + strings.Repeat("]", 5000))

	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("глубокое тело: status = %d, want 400", resp.StatusCode)
	}

	// Процесс жив: следующий запрос обслуживается как обычно.
	ok := s.postOTLP(t, []byte(`{"resourceSpans":[]}`), "application/json", s.key.PublicKey, false)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("после глубокого тела приём отвечает %d — процесс не пережил", ok.StatusCode)
	}
}

func TestOTLPJSONAcceptsNormalDepth(t *testing.T) {
	s := newStack(t)
	body := otlpJSONBodyHexIDs(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertOTLPRows(t, s) // среди прочего сверяет trace_id/span_id — доказательство, что id подменены, а не испорчены
}

func TestOTLPDeepBodyRejectionIsLogged(t *testing.T) {
	s := newStack(t)

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	body := []byte(strings.Repeat("[", 5000) + strings.Repeat("]", 5000))
	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	out := logs.String()
	if !strings.Contains(out, "reason=json_too_deep") {
		t.Errorf("лог не содержит reason=json_too_deep:\n%s", out)
	}
	if !strings.Contains(out, "project_id=") {
		t.Errorf("лог не называет project_id:\n%s", out)
	}
}
