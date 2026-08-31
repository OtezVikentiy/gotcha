package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// recordingMux — подменный регистратор: http.ServeMux не даёт перечислить
// зарегистрированные паттерны, а сторожу нужно именно это.
//
// Сигнатура HandleFunc здесь — func(http.ResponseWriter, *http.Request), а не
// именованный http.HandlerFunc: интерфейс muxRegistrar требует ИДЕНТИЧНОГО
// типа параметра с *http.ServeMux.HandleFunc (тот принимает безымянный
// func-тип), и именованный тип ему не удовлетворяет — компилятор это не
// пропускает.
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

// ingestRoute — ожидаемый маршрут приёма: паттерн, конкретный URL для
// запроса, сигнал маршрута и тип ключа, которому здесь быть НЕ должно.
// denied == "" означает маршрут БЕЗ аутентификации (CORS-preflight) — такие
// проверяются только на присутствие в таблице.
type ingestRoute struct {
	pattern string
	path    string
	signal  IngestSignal
	denied  org.KeyKind
}

// wantIngestRoutes — ПОЛНЫЙ список входов приёма. Таблица живёт в тесте, а не
// в коде: Register снабжён содержательными комментариями по каждому маршруту,
// переписывать его ради перечислимости не нужно.
//
// Новый эндпойнт приёма, не внесённый сюда, роняет тест. Маршрут, внесённый,
// но забывший гейт скоупа, роняет тест тоже — без второй проверки сторож ловил
// бы только появление маршрута, но не отсутствие на нём гейта, то есть ровно
// ту ошибку, ради которой заводится.
var wantIngestRoutes = []ingestRoute{
	{"POST /api/{project}/envelope/{$}", "/api/7/envelope/", SignalEvent, org.KindAgent},
	{"OPTIONS /api/{project}/envelope/{$}", "/api/7/envelope/", "", ""},
	{"POST /api/{project}/store/{$}", "/api/7/store/", SignalEvent, org.KindAgent},
	{"OPTIONS /api/{project}/store/{$}", "/api/7/store/", "", ""},
	{"POST /v1/traces", "/v1/traces", SignalTransaction, org.KindAgent},
	// /v1/metrics открыт ВСЕМ четырём известным типам — запрещает его только
	// незаданный тип. Именно им сторож сюда и стучит: маршрут без гейта
	// пропустил бы ключ, у которого типа нет вовсе.
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
	}
	for p := range got {
		if !want[p] {
			t.Errorf("зарегистрирован маршрут приёма вне таблицы: %s — внесите его и убедитесь, что на нём стоит гейт скоупа", p)
		}
	}
}

// TestIngestRoutesScopeGated — на КАЖДОМ аутентифицируемом маршруте ключ
// заведомо неподходящего типа получает 403 и инкрементирует счётчик скоупа.
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
			// Пайплайн — НЕ nil: при снятом (в мутационной проверке) гейте
			// скоупа otlp-маршруты уходят вглубь обработчика и обращаются к
			// h.Pipeline (например, Pipeline.TracingEnabled в otlpTraces).
			// С nil-пайплайном это паника без recover, которая убивает весь
			// процесс go test — тогда при снятом гейте падает и отчитывается
			// только ПЕРВЫЙ маршрут таблицы, а остальные шесть подтестов
			// просто не запускаются, и сторож перестаёт называть виновника.
			// NewPipeline(nil, nil) достаточно: до записи дело не доходит,
			// гейт (когда он на месте) отбивает запрос раньше.
			h := NewHandler(NewKeyCache(fr), nil, NewPipeline(nil, nil), 1<<20)
			mux := http.NewServeMux()
			h.Register(mux)

			req := httptest.NewRequest("POST", route.path+"?sentry_key=k", strings.NewReader("{}"))
			// Оба способа предъявления ключа сразу: Sentry-вход читает
			// sentry_key, OTLP-вход — Bearer. Тест не обязан знать, какой из
			// них у конкретного маршрута.
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
