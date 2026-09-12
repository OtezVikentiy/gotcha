package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

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
	if scopeAllows(org.KeyKind("root"), SignalEvent) || scopeAllowsHosts(org.KeyKind("root")) {
		t.Error("неизвестный тип ключа получил допуск")
	}
}

func TestScopeAllowsRoute(t *testing.T) {
	if !scopeAllowsRoute(org.KindBrowser, SignalEvent, envelopeAlsoSignals) {
		t.Error("browser не пустили в envelope")
	}
	if scopeAllowsRoute(org.KindAgent, SignalEvent, envelopeAlsoSignals) {
		t.Error("agent пустили в envelope")
	}
	if scopeAllowsRoute(org.KeyKind(""), SignalEvent, envelopeAlsoSignals) {
		t.Error("незаданный тип пустили в envelope")
	}
}

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

func TestEnvelopeBrowserProfileRejected(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"browserkey": {ID: 2, ProjectID: 7, OrgID: 3, PublicKey: "browserkey", Kind: org.KindBrowser},
	}}
	h := NewHandler(NewKeyCache(fr), nil, NewPipeline(nil, nil), 1<<20)
	raw := strings.Join([]string{
		`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}`,
		`{"type":"event"}`, `{"message":"e"}`,
		`{"type":"profile"}`, `{"profile":"p"}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/7/envelope/?sentry_key=browserkey", strings.NewReader(raw))
	req.SetPathValue("project", "7")

	h.envelope(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200; тело: %s", rec.Code, rec.Body.String())
	}
	if got := h.pipeline.Queued(); got != 1 {
		t.Fatalf("событие не доехало до приёмника: очередь = %d, ожидалась 1", got)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalProfile); got != 1 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,profile} = %d, ожидалась 1", got)
	}
	if got := h.RejectedBy(RejectKeyScope, SignalEvent); got != 0 {
		t.Fatalf("gotcha_ingest_rejected_total{key_scope,event} = %d, ожидалась 0", got)
	}
}

func TestOTLPMetricsHostScope(t *testing.T) {
	t.Run("server", func(t *testing.T) {
		sink := &collectMetricSink{}
		hosts := newFakeHostRegistry()
		h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindServer}}), nil, nil, 1<<20)
		h.Metrics = sink
		h.Hosts = hosts

		w := postOTLPMetrics(t, h, []*metricspb.ResourceMetrics{
			resourceMetricWithHost("web-1", "cpu"),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if len(sink.points) != 1 {
			t.Fatalf("точек записано = %d, want 1 (экспорт принят)", len(sink.points))
		}
		if got := hosts.get(1); len(got) != 0 {
			t.Fatalf("Toucher не вызывался: получено %v", got)
		}
		if got := h.HostScopeSkipped(); got != 1 {
			t.Fatalf("gotcha_host_registrations_scope_skipped_total = %d, ожидалась 1", got)
		}
	})

	t.Run("agent", func(t *testing.T) {
		sink := &collectMetricSink{}
		hosts := newFakeHostRegistry()
		h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindAgent}}), nil, nil, 1<<20)
		h.Metrics = sink
		h.Hosts = hosts

		w := postOTLPMetrics(t, h, []*metricspb.ResourceMetrics{
			resourceMetricWithHost("web-1", "cpu"),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := hosts.get(1); len(got) != 1 || got[0] != "web-1" {
			t.Fatalf("хост не зарегистрирован: %v, ожидался [web-1]", got)
		}
		if got := h.HostScopeSkipped(); got != 0 {
			t.Fatalf("gotcha_host_registrations_scope_skipped_total = %d, ожидалась 0", got)
		}
	})

	t.Run("server без host.*", func(t *testing.T) {
		sink := &collectMetricSink{}
		hosts := newFakeHostRegistry()
		h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindServer}}), nil, nil, 1<<20)
		h.Metrics = sink
		h.Hosts = hosts

		w := postOTLPMetrics(t, h, []*metricspb.ResourceMetrics{
			resourceMetricWithHost("", "requests"),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if len(sink.points) != 1 {
			t.Fatalf("точек записано = %d, want 1 (экспорт принят)", len(sink.points))
		}
		if got := hosts.get(1); len(got) != 0 {
			t.Fatalf("Toucher не должен вызываться: получено %v", got)
		}
		if got := h.HostScopeSkipped(); got != 0 {
			t.Fatalf("gotcha_host_registrations_scope_skipped_total = %d, ожидалась 0 (батч без host.*)", got)
		}
	})

	t.Run("server несколько точек одного хоста", func(t *testing.T) {
		sink := &collectMetricSink{}
		hosts := newFakeHostRegistry()
		h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindServer}}), nil, nil, 1<<20)
		h.Metrics = sink
		h.Hosts = hosts

		w := postOTLPMetrics(t, h, []*metricspb.ResourceMetrics{
			resourceMetricWithHost("web-1", "cpu"),
			resourceMetricWithHost("web-1", "mem"),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := h.HostScopeSkipped(); got != 1 {
			t.Fatalf("gotcha_host_registrations_scope_skipped_total = %d, ожидалась 1 (один экспорт, а не число точек)", got)
		}
	})
}
