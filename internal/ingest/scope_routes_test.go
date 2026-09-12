package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type recordingMux struct {
	patterns []string
	handlers map[string]func(http.ResponseWriter, *http.Request)
}

func (m *recordingMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	if m.handlers == nil {
		m.handlers = map[string]func(http.ResponseWriter, *http.Request){}
	}
	m.handlers[pattern] = h
}

type ingestRoute struct {
	pattern string
	path    string
	signal  IngestSignal
	denied  org.KeyKind
}

var wantIngestRoutes = []ingestRoute{
	{"POST /api/{project}/envelope/{$}", "/api/7/envelope/", SignalEvent, org.KindAgent},
	{"OPTIONS /api/{project}/envelope/{$}", "/api/7/envelope/", "", ""},
	{"POST /api/{project}/store/{$}", "/api/7/store/", SignalEvent, org.KindAgent},
	{"OPTIONS /api/{project}/store/{$}", "/api/7/store/", "", ""},
	{"POST /v1/traces", "/v1/traces", SignalTransaction, org.KindAgent},
	{"POST /v1/metrics", "/v1/metrics", SignalMetric, org.KeyKind("nokind")},
	{"POST /api/v1/profiles/pprof", "/api/v1/profiles/pprof", SignalProfile, org.KindBrowser},
	{"POST /profiles/pprof", "/profiles/pprof", SignalProfile, org.KindBrowser},
	{"POST /v1/logs", "/v1/logs", SignalLog, org.KindAgent},
	{"POST /api/v1/logs", "/api/v1/logs", SignalLog, org.KindAgent},
	{"POST /logs", "/logs", SignalLog, org.KindAgent},
	{"POST /api/v1/{project}/deployments", "/api/v1/7/deployments", SignalDeploy, org.KindBrowser},
	{"POST /api/v1/{project}/deployments/{$}", "/api/v1/7/deployments/", SignalDeploy, org.KindBrowser},
	{"POST /api/{project}/deployments/{$}", "/api/7/deployments/", SignalDeploy, org.KindBrowser},
}

func TestIngestRoutesGuard(t *testing.T) {
	rec := &recordingMux{}
	(&Handler{}).Register(rec)

	got := map[string]bool{}
	for _, p := range rec.patterns {
		if got[p] {
			t.Errorf("паттерн зарегистрирован дважды: %s", p)
		}
		got[p] = true
	}
	want := map[string]bool{}
	for _, r := range wantIngestRoutes {
		want[r.pattern] = true
		if !got[r.pattern] {
			t.Errorf("маршрут из таблицы не зарегистрирован: %s", r.pattern)
		}
		if r.denied == "" && !strings.HasPrefix(r.pattern, "OPTIONS ") {
			t.Errorf("маршрут %s: пустой denied допустим только у OPTIONS-preflight — заполните denied, чтобы TestIngestRoutesScopeGated проверил гейт", r.pattern)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("зарегистрирован маршрут приёма вне таблицы: %s — внесите его и убедитесь, что на нём стоит гейт скоупа", p)
		}
	}
}

func TestIngestRoutesScopeGated(t *testing.T) {
	for _, route := range wantIngestRoutes {
		if route.denied == "" {
			continue
		}
		t.Run(route.pattern, func(t *testing.T) {
			kind := route.denied
			if kind == "nokind" {
				kind = ""
			}
			fr := &fakeResolver{keys: map[string]org.Key{
				"k": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "k", Kind: kind},
			}}
			h := NewHandler(NewKeyCache(fr), nil, NewPipeline(nil, nil), 1<<20)
			mux := http.NewServeMux()
			h.Register(mux)

			req := httptest.NewRequest("POST", route.path+"?sentry_key=k", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer k")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("статус %d, ожидался 403 — на маршруте нет гейта скоупа", w.Code)
			}
			if n := h.RejectedBy(RejectKeyScope, route.signal); n != 1 {
				t.Fatalf("gotcha_ingest_rejected_total{key_scope,%s} = %d, ожидалась 1", route.signal, n)
			}
		})
	}
}
