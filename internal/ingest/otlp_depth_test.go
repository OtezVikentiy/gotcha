package ingest_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestOTLPJSONRejectsDeepBody — ЗАКРЫВАЕМАЯ ДЫРА (P0 №3 аудита 2026-07-30).
//
// otlpIDRewriter обходил тело OTLP/JSON взаимной рекурсией (value → valueFrom
// → object|array → value) без предела глубины: тело вида [[[…]]] разворачивало
// стек горутины Go (растёт до 1 ГБ) до fatal error: stack overflow — ошибки
// рантайма, которую recover не ловит, — и умирал весь процесс целиком, а в
// режиме `all` это одновременно веб, приём и аптайм.
//
// На этой машине воспроизвести падение НАПРЯМУЮ вызовом otlpJSONHexIDs (с
// намеренно отключенной проверкой глубины) удалось только на глубине ПОРЯДКА
// 5 000 000 вложенных массивов — 5000/50000/500000/3000000 переживались без
// падения. Такая глубина реалистично достижима через этот HTTP-путь: тело
// "[[[…]]]" сжимается gzip'ом в считаные килобайты, а лимит на РАСПАКОВАННОЕ
// тело — 10×maxBytes (см. Handler.body), то есть 10 МиБ по умолчанию, — почти
// точно вмещает такую глубину. Здесь же тело шлётся МАЛЕНЬКИМ (5000 уровней,
// заведомо больше maxJSONWalkDepth=100), потому что после фикса предел обхода
// срабатывает почти сразу на глубине 101 — величина тела на исход теста не
// влияет, а маленькое тело быстрее.
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

// TestOTLPJSONAcceptsNormalDepth — предел обхода не задевает настоящие тела:
// реальная глубина OTLP/JSON (resourceSpans → scopeSpans → spans → attributes →
// value) около десяти, заведомо меньше maxJSONWalkDepth=100. Проверяем не
// только код ответа, а то, что подмена hex→base64 при этом продолжает
// работать (см. докблок otlpJSONHexIDs) — регрессия здесь означала бы, что
// предел обхода где-то задевает и обычные тела.
func TestOTLPJSONAcceptsNormalDepth(t *testing.T) {
	s := newStack(t)
	body := otlpJSONBodyHexIDs(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertOTLPRows(t, s) // среди прочего сверяет trace_id/span_id — доказательство, что id подменены, а не испорчены
}

// TestOTLPDeepBodyRejectionIsLogged — отказ по глубине пишет отдельную
// причину (reason=json_too_deep), которую эксплуатация отличает от обычного
// malformed-payload: превышение предела обхода — НЕ обычная ошибка разбора
// (см. errJSONTooDeep), и должно быть видно в логах отдельно.
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
