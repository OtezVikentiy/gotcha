package ingest

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// recordedTouch — один вызов SignalRecorder.Touch, зафиксированный
// fakeSignalRecorder.
type recordedTouch struct {
	projectID int64
	kind      ingestsignal.Kind
}

// fakeSignalRecorder — Recorder-заглушка: копит вызовы Touch вместо записи в
// БД. mu — authenticate/scopeReject вызываются из обработчика HTTP-запроса,
// который в бою конкурентен; заглушка не должна давать гонку данных под -race.
type fakeSignalRecorder struct {
	mu      sync.Mutex
	touches []recordedTouch
}

func (f *fakeSignalRecorder) Touch(projectID int64, kind ingestsignal.Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, recordedTouch{projectID, kind})
}

// TestAuthenticateRecordsKeyRejectSignals — четыре причины отказа
// authenticate дают четыре разных Touch: missing_key и invalid_key сводятся к
// одному виду сигнала (KindKeyInvalid) с project id ИЗ URL — ключ либо
// отсутствует, либо не резолвится, и до какого проекта он вообще не долетел,
// неизвестно; project_mismatch и key_scope несут key.ProjectID — ключ
// резолвился, проект известен точно.
func TestAuthenticateRecordsKeyRejectSignals(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"validkey": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "validkey", Kind: org.KindServer},
		"agentkey": {ID: 2, ProjectID: 9, OrgID: 3, PublicKey: "agentkey", Kind: org.KindAgent},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	sig := &fakeSignalRecorder{}
	h.Signals = sig

	call := func(path, project string) {
		req := httptest.NewRequest("POST", path, nil)
		req.SetPathValue("project", project)
		rec := httptest.NewRecorder()
		if _, ok := h.authenticate(rec, req, SignalEvent, envelopeAlsoSignals...); ok {
			t.Fatalf("%s: аутентификация неожиданно прошла", path)
		}
	}

	// missing_key: sentry_key вовсе не прислан, project id только из URL — 7.
	call("/api/7/envelope/", "7")
	// invalid_key: ключ прислан, но не резолвится — project id снова из URL.
	call("/api/7/envelope/?sentry_key=nope", "7")
	// project_mismatch: validkey резолвится в проект 7, а URL просит проект 8.
	call("/api/8/envelope/?sentry_key=validkey", "8")
	// key_scope: agentkey резолвится в проект 9, но agent не допущен в envelope.
	call("/api/9/envelope/?sentry_key=agentkey", "9")

	want := []recordedTouch{
		{7, ingestsignal.KindKeyInvalid},
		{7, ingestsignal.KindKeyInvalid},
		{7, ingestsignal.KindKeyProjectMismatch}, // key.ProjectID=7, НЕ 8 из URL
		{9, ingestsignal.KindKeyScope},
	}
	if !reflect.DeepEqual(sig.touches, want) {
		t.Errorf("touches = %+v, want %+v", sig.touches, want)
	}
}

// TestAuthenticateSignalsNilSafe — Signals==nil (значение по умолчанию) не
// паникует ни на одной ветке отказа, ни на успехе с устаревшим путём.
func TestAuthenticateSignalsNilSafe(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{
		"validkey": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "validkey", Kind: org.KindServer},
	}}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	// h.Signals остаётся nil.

	req := httptest.NewRequest("POST", "/api/7/envelope/", nil)
	req.SetPathValue("project", "7")
	h.authenticate(httptest.NewRecorder(), req, SignalEvent, envelopeAlsoSignals...) // missing_key

	req = httptest.NewRequest("POST", "/api/7/envelope/?sentry_key=validkey", nil)
	req.SetPathValue("project", "7")
	if _, ok := h.authenticate(httptest.NewRecorder(), req, SignalEvent, envelopeAlsoSignals...); !ok {
		t.Fatal("валидный ключ не прошёл аутентификацию")
	}
}

// TestDeprecatedAliasRecordsSignalForProject — K7-5/K7-6: попадание на
// устаревший путь и провал аутентификации на другом устаревшем пути дают
// разные, не путающиеся друг с другом сигналы.
func TestDeprecatedAliasRecordsSignalForProject(t *testing.T) {
	t.Run("алиас /logs, валидный ключ — deprecated_logs на проект ключа", func(t *testing.T) {
		h := newRejectHandler(1 << 20) // stubKeyResolver: любой Bearer → project 1
		h.Logs = &collectLogSink{}
		sig := &fakeSignalRecorder{}
		h.Signals = sig

		resp := serveIngest(h, ndjsonRequest("/logs"))
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
		}

		want := []recordedTouch{{1, ingestsignal.KindDeprecatedLogs}}
		if !reflect.DeepEqual(sig.touches, want) {
			t.Errorf("touches = %+v, want %+v", sig.touches, want)
		}
	})

	t.Run("алиас деплоев, невалидный ключ — key_invalid с проектом из URL, deprecated-сигнала нет", func(t *testing.T) {
		fr := &fakeResolver{keys: map[string]org.Key{}} // ни один ключ не резолвится
		h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
		sig := &fakeSignalRecorder{}
		h.Signals = sig

		req := httptest.NewRequest("POST", "/api/7/deployments/?sentry_key=nope", strings.NewReader(`{}`))
		resp := serveIngest(h, req)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body = %s", resp.Code, resp.Body.String())
		}

		want := []recordedTouch{{7, ingestsignal.KindKeyInvalid}}
		if !reflect.DeepEqual(sig.touches, want) {
			t.Errorf("touches = %+v, want %+v (аутентификация провалилась ДО DeprecatedPath-проверки)", sig.touches, want)
		}
	})

	t.Run("алиас деплоев, валидный ключ — deprecated_deployments на проект ключа", func(t *testing.T) {
		fr := &fakeResolver{keys: map[string]org.Key{
			"validkey": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "validkey", Kind: org.KindServer},
		}}
		h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
		sig := &fakeSignalRecorder{}
		h.Signals = sig
		// h.Deploy остаётся nil: аутентификация (и, значит, touchDeprecatedSignal)
		// проходит раньше проверки h.Deploy == nil в deploymentsIngest — 503
		// приходит ПОСЛЕ того, как сигнал уже отмечен.

		req := httptest.NewRequest("POST", "/api/7/deployments/?sentry_key=validkey", strings.NewReader(`{}`))
		resp := serveIngest(h, req)
		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (Deploy не настроен), body = %s", resp.Code, resp.Body.String())
		}

		want := []recordedTouch{{7, ingestsignal.KindDeprecatedDeployments}}
		if !reflect.DeepEqual(sig.touches, want) {
			t.Errorf("touches = %+v, want %+v (валидный ключ на устаревшем пути деплоев обязан дать deprecated_deployments)", sig.touches, want)
		}
	})
}
