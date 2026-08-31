package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestKeyScopeMatrix перебирает матрицу §3.1 спеки ЦЕЛИКОМ: пять строк
// (четыре типа плюс незаданный) на шесть сигналов плюс предикат hosts.
// Таблица выписана здесь ВРУЧНУЮ и намеренно не переиспользует
// keyScopeMatrix: тест, читающий ту же переменную, что и код, подтверждал бы
// лишь её саму себя.
func TestKeyScopeMatrix(t *testing.T) {
	type row struct {
		kind  org.KeyKind
		allow map[IngestSignal]bool
		hosts bool
	}
	rows := []row{
		{org.KindBrowser, map[IngestSignal]bool{
			SignalEvent: true, SignalTransaction: true, SignalMetric: true,
			SignalLog: true, SignalProfile: false, SignalDeploy: false,
		}, false},
		{org.KindServer, map[IngestSignal]bool{
			SignalEvent: true, SignalTransaction: true, SignalMetric: true,
			SignalLog: true, SignalProfile: true, SignalDeploy: true,
		}, false},
		{org.KindAgent, map[IngestSignal]bool{
			SignalEvent: false, SignalTransaction: false, SignalMetric: true,
			SignalLog: false, SignalProfile: false, SignalDeploy: false,
		}, true},
		{org.KindLegacy, map[IngestSignal]bool{
			SignalEvent: true, SignalTransaction: true, SignalMetric: true,
			SignalLog: true, SignalProfile: true, SignalDeploy: true,
		}, true},
		// Незаданный тип — ОТКАЗ по всему, а не полный допуск: забытое
		// значение в будущем коде не должно выдавать права (§3.1 спеки).
		{org.KeyKind(""), map[IngestSignal]bool{
			SignalEvent: false, SignalTransaction: false, SignalMetric: false,
			SignalLog: false, SignalProfile: false, SignalDeploy: false,
		}, false},
	}
	for _, r := range rows {
		for signal, want := range r.allow {
			if got := scopeAllows(r.kind, signal); got != want {
				t.Errorf("scopeAllows(%q, %q) = %v, ожидалось %v", r.kind, signal, got, want)
			}
		}
		if got := scopeAllowsHosts(r.kind); got != r.hosts {
			t.Errorf("scopeAllowsHosts(%q) = %v, ожидалось %v", r.kind, got, r.hosts)
		}
	}
	// Неизвестный тип (значение, которого нет ни в CHECK, ни в константах)
	// тоже отказ: матрица закрыта, а не «всё, кроме перечисленного».
	if scopeAllows(org.KeyKind("root"), SignalEvent) || scopeAllowsHosts(org.KeyKind("root")) {
		t.Error("неизвестный тип ключа получил допуск")
	}
}

// TestScopeAllowsRoute — мультисигнальный маршрут (envelope) открыт, если
// разрешён ХОТЯ БЫ ОДИН из сигналов, которые он может нести.
func TestScopeAllowsRoute(t *testing.T) {
	// browser не допущен к профилям, но envelope ему открыт: внутри он несёт
	// события, а профили отбираются поштучно вторичным гейтом.
	if !scopeAllowsRoute(org.KindBrowser, SignalEvent, envelopeAlsoSignals) {
		t.Error("browser не пустили в envelope")
	}
	// agent не допущен ни к одному из сигналов envelope'а.
	if scopeAllowsRoute(org.KindAgent, SignalEvent, envelopeAlsoSignals) {
		t.Error("agent пустили в envelope")
	}
	// Незаданный тип не открывает ничего.
	if scopeAllowsRoute(org.KeyKind(""), SignalEvent, envelopeAlsoSignals) {
		t.Error("незаданный тип пустили в envelope")
	}
}

// TestKeyScopeRejectionPairsCoverMatrix — пары (key_scope, signal)
// ВЫЧИСЛЯЮТСЯ из матрицы, а не выписаны руками. Пара, которой нет в наборе,
// молча не инкрементируется (countRejected), то есть отсутствие пары даёт
// fail-silent — поэтому проверяется, что покрыт КАЖДЫЙ сигнал, который хоть
// одна строка матрицы (включая незаданный тип) запрещает.
func TestKeyScopeRejectionPairsCoverMatrix(t *testing.T) {
	got := map[IngestSignal]bool{}
	for _, p := range keyScopeRejectionPairs() {
		if p.Reason != RejectKeyScope {
			t.Fatalf("чужая причина в парах скоупа: %q", p.Reason)
		}
		got[p.Signal] = true
	}
	for _, s := range allIngestSignals {
		if !got[s] {
			t.Errorf("сигнал %q не покрыт парой (key_scope, %q): отказ по нему не будет посчитан", s, s)
		}
	}
	if len(got) != len(allIngestSignals) {
		t.Errorf("пар %d, сигналов %d", len(got), len(allIngestSignals))
	}
}

// TestAuthenticateScopeGate — Sentry-вход: ключ агента в envelope получает
// 403 и инкрементирует ОБА счётчика (gotcha_ingest_rejected_total{key_scope}
// и gotcha_ingest_key_rejections_total{scope}).
func TestAuthenticateScopeGate(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"agentkey": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "agentkey", Kind: org.KindAgent},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/7/envelope/?sentry_key=agentkey", nil)
	req.SetPathValue("project", "7")

	if _, ok := h.authenticate(rec, req, SignalEvent, envelopeAlsoSignals...); ok {
		t.Fatal("agent-ключ пропущен в envelope")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("статус %d, ожидался 403", rec.Code)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalEvent); got != 1 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,event} = %d, ожидалась 1", got)
	}
	if got := h.KeyRejectedBy(KeyRejectScope); got != 1 {
		t.Fatalf("gotcha_ingest_key_rejections_total{scope} = %d, ожидалась 1", got)
	}
}

// TestAuthenticateScopeGateAllows — тот же вход, но ключ браузера: проходит,
// хотя профили ему запрещены (envelope мультисигнален).
func TestAuthenticateScopeGateAllows(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"browserkey": {ID: 2, ProjectID: 7, OrgID: 3, PublicKey: "browserkey", Kind: org.KindBrowser},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/7/envelope/?sentry_key=browserkey", nil)
	req.SetPathValue("project", "7")

	if _, ok := h.authenticate(rec, req, SignalEvent, envelopeAlsoSignals...); !ok {
		t.Fatal("browser-ключ не пропущен в envelope")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200 (ничего не писалось)", rec.Code)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalEvent); got != 0 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,event} = %d, ожидалась 0", got)
	}
	if got := h.KeyRejectedBy(KeyRejectScope); got != 0 {
		t.Fatalf("gotcha_ingest_key_rejections_total{scope} = %d, ожидалась 0", got)
	}
}

// TestOTLPAuthenticateScopeGate — OTLP-вход: браузерный ключ в /v1/traces
// проходит, а ключ агента получает 403 (а НЕ 401: ключ резолвился успешно,
// это «сюда нельзя», а не «ты не представился»).
func TestOTLPAuthenticateScopeGate(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"agentkey": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "agentkey", Kind: org.KindAgent},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer agentkey")

	if _, ok := h.otlpAuthenticate(rec, req, SignalTransaction); ok {
		t.Fatal("agent-ключ пропущен в /v1/traces")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("статус %d, ожидался 403", rec.Code)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalTransaction); got != 1 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,transaction} = %d, ожидалась 1", got)
	}
}

// TestAuthenticateEmptyKindDenied — ключ с незаданным типом не проходит
// НИКУДА, включая метрики, разрешённые всем известным типам.
func TestAuthenticateEmptyKindDenied(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"nokind": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "nokind"},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer nokind")

	if _, ok := h.otlpAuthenticate(rec, req, SignalMetric); ok {
		t.Fatal("ключ с незаданным типом пропущен в /v1/metrics")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("статус %d, ожидался 403", rec.Code)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalMetric); got != 1 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,metric} = %d, ожидалась 1", got)
	}
}
