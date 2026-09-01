package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestParseDeployTime — прямой разбор поля deployed_at: три допустимые формы
// (RFC3339-строка, Unix-секунды числом, отсутствие/null) плюс мусор от кривого
// CI. Всё непонятное → нулевое время (Record подставит now()), БЕЗ паники.
func TestParseDeployTime(t *testing.T) {
	wantRFC := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := []struct {
		name string
		raw  string
		zero bool
		want time.Time
	}{
		{"rfc3339", `"2026-01-02T03:04:05Z"`, false, wantRFC},
		{"unix-seconds", `1735780800`, false, time.Unix(1735780800, 0).UTC()},
		{"empty", ``, true, time.Time{}},
		{"null", `null`, true, time.Time{}},
		{"object", `{"x":1}`, true, time.Time{}},
		{"bool", `true`, true, time.Time{}},
		{"fractional", `1.5`, true, time.Time{}},
		{"non-time-string", `"not-a-time"`, true, time.Time{}},
		// Значение НАЧИНАЕТСЯ с кавычки, но JSON-строкой не является
		// (незакрытая кавычка): снятие кавычек честным декодом обязано
		// вернуть нулевое время, а не запаниковать на обрезке байтов.
		{"unterminated-string", `"2026-01-02T03:04:05Z`, true, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDeployTime(json.RawMessage(tc.raw))
			if tc.zero {
				if !got.IsZero() {
					t.Fatalf("parseDeployTime(%q) = %s, want zero", tc.raw, got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Fatalf("parseDeployTime(%q) = %s, want %s", tc.raw, got, tc.want)
			}
			// Unix-число разбирается именно в UTC (маркеры графиков в UTC).
			if got.Location() != time.UTC {
				t.Errorf("parseDeployTime(%q) в зоне %s, want UTC", tc.raw, got.Location())
			}
		})
	}
}

// newIngestTestWithDeploy строит ingest-хендлер с КОНКРЕТНЫМ deploy.Store поверх
// мигрированной PG и засеянными org/project. Ключ авторизации указывает на тот же
// project id (FK deployments.project_id → projects.id), иначе Record упадёт на FK.
func newIngestTestWithDeploy(t *testing.T) (h *Handler, projectID int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('c5-test', 'C5 Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'c5-test', 'C5 Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	h = NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: projectID, OrgID: orgID, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	h.Deploy = deploy.NewStore(pool)
	return h, projectID
}

// postDeploy шлёт POST /api/v1/{project}/deployments с sentry_key и телом body.
func postDeploy(t *testing.T, h *Handler, projectID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST",
		"/api/v1/"+strconv.FormatInt(projectID, 10)+"/deployments?sentry_key=deadbeef", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestIngestDeployment(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)

	body := `{"version":"v2.0.0","environment":"prod","url":"https://ci/run/9","changelog":"fix X"}`
	rec := postDeploy(t, h, projectID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	deps, err := h.Deploy.Recent(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(deps) != 1 || deps[0].Version != "v2.0.0" {
		t.Fatalf("не записалось: %+v", deps)
	}
	if deps[0].Environment != "prod" || deps[0].URL != "https://ci/run/9" {
		t.Fatalf("поля не сохранились: %+v", deps[0])
	}

	// version пусто → 400, запись не добавляется
	bad := postDeploy(t, h, projectID, `{"environment":"prod"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("пустой version → %d, want 400", bad.Code)
	}

	// malformed json → 400
	broken := postDeploy(t, h, projectID, `{not-json`)
	if broken.Code != http.StatusBadRequest {
		t.Fatalf("битый json → %d, want 400", broken.Code)
	}

	deps2, _ := h.Deploy.Recent(context.Background(), projectID, 10)
	if len(deps2) != 1 {
		t.Fatalf("невалидные запросы не должны писать: %+v", deps2)
	}
}

// TestIngestDeploymentDeployedAt проверяет разбор явного deployed_at (RFC3339).
func TestIngestDeploymentDeployedAt(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)

	body := `{"version":"v9","deployed_at":"2026-01-02T03:04:05Z"}`
	rec := postDeploy(t, h, projectID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	deps, _ := h.Deploy.Recent(context.Background(), projectID, 10)
	if len(deps) != 1 {
		t.Fatalf("нет записи: %+v", deps)
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !deps[0].DeployedAt.Equal(want) {
		t.Fatalf("deployed_at = %s, want %s", deps[0].DeployedAt.UTC(), want)
	}
}

// TestIngestDeploymentTooLarge: тело деплоя сверх лимита (GOTCHA_MAX_EVENT_BYTES)
// обязано отвечать 413, а не 400 — decode-ошибка от json.Decoder на теле,
// упёршемся в http.MaxBytesReader, раньше НЕ отличалась от битого JSON (см.
// докблок в deploymentsIngest). Заодно проверяет self-метрику T6: (too_large,
// deploy) — одна из 29 пар gotcha_ingest_rejected_total, ничем не защищённых.
func TestIngestDeploymentTooLarge(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)
	before := h.RejectedBy(RejectTooLarge, SignalDeploy)

	big := `{"version":"` + strings.Repeat("a", 2<<20) + `"}`
	rec := postDeploy(t, h, projectID, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status=%d, want 413, body=%s", rec.Code, rec.Body.String())
	}
	if got := h.RejectedBy(RejectTooLarge, SignalDeploy); got != before+1 {
		t.Fatalf("RejectedBy(too_large, deploy) = %d, want %d", got, before+1)
	}

	deps, err := h.Deploy.Recent(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("отклонённый деплой не должен попасть в стор: %+v", deps)
	}
}

// TestIngestDeploymentDisabled: без сконфигурированного стора эндпоинт отвечает 503.
func TestIngestDeploymentDisabled(t *testing.T) {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	rec := postDeploy(t, h, 1, `{"version":"v1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("h.Deploy==nil → %d, want 503", rec.Code)
	}
}

// TestIngestDeploymentRateLimited: эндпоинт деплоя троттлится тем же per-DSN
// лимитером, что envelope/store (публичный sentry_key → без rate-limit безлимитный
// поток INSERT'ов). Фиксированные часы + burst 1: первый запрос проходит, второй
// в тот же миг — 429, и вторая запись НЕ появляется в сторе.
func TestIngestDeploymentRateLimited(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)
	now := time.Unix(0, 0)
	h.SetRateLimit(func() time.Time { return now }, 1, 1) // 1 ток/с, запас 1

	if rec := postDeploy(t, h, projectID, `{"version":"v1.0.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("первый запрос: status=%d, want 200", rec.Code)
	}
	before := h.RejectedBy(RejectRateLimit, SignalDeploy)
	if rec := postDeploy(t, h, projectID, `{"version":"v1.0.1"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос: status=%d, want 429", rec.Code)
	}
	if got := h.RejectedBy(RejectRateLimit, SignalDeploy); got != before+1 {
		t.Errorf("RejectedBy(rate_limit, deploy) = %d, want %d", got, before+1)
	}

	deps, err := h.Deploy.Recent(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("после 429 в сторе должна быть 1 запись, got %d: %+v", len(deps), deps)
	}
}

// deployRequest — POST /api/v1/{project}/deployments с произвольными заголовками
// и (опционально) без sentry_key: postDeploy ключ подставляет всегда, а ветки
// отказа по ключу и по кодировке тела иначе недостижимы.
func deployRequest(h *Handler, projectID int64, body string, withKey bool, contentEncoding string) *httptest.ResponseRecorder {
	url := "/api/v1/" + strconv.FormatInt(projectID, 10) + "/deployments"
	if withKey {
		url += "?sentry_key=deadbeef"
	}
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

// TestIngestDeploymentUnauthorized: запрос без sentry_key → 401, и отказ виден
// self-метрикой с сигналом deploy — приёмник обязан различать, какой ИЗ ШЕСТИ
// входов отбивало по ключу, иначе дежурный видит только общий рост 401.
func TestIngestDeploymentUnauthorized(t *testing.T) {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	before := h.RejectedBy(RejectKeyUnknown, SignalDeploy)

	rec := deployRequest(h, 1, `{"version":"v1"}`, false, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без sentry_key: status=%d, want 401", rec.Code)
	}
	if got := h.RejectedBy(RejectKeyUnknown, SignalDeploy); got != before+1 {
		t.Errorf("RejectedBy(key_unknown, deploy) = %d, want %d", got, before+1)
	}
}

// TestIngestDeploymentBadBodyEncoding: Content-Encoding: gzip на не-gzip теле —
// h.body падает до разбора JSON → 400 с причиной malformed (не too_large:
// тело в лимит уложилось, испорчена именно кодировка).
func TestIngestDeploymentBadBodyEncoding(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)
	beforeBad := h.RejectedBy(RejectMalformed, SignalDeploy)
	beforeLarge := h.RejectedBy(RejectTooLarge, SignalDeploy)

	rec := deployRequest(h, projectID, "это не gzip", true, "gzip")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if got := h.RejectedBy(RejectMalformed, SignalDeploy); got != beforeBad+1 {
		t.Errorf("RejectedBy(malformed, deploy) = %d, want %d", got, beforeBad+1)
	}
	if got := h.RejectedBy(RejectTooLarge, SignalDeploy); got != beforeLarge {
		t.Errorf("RejectedBy(too_large, deploy) = %d, want %d (битая кодировка — не превышение лимита)", got, beforeLarge)
	}

	deps, err := h.Deploy.Recent(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("отклонённый запрос не должен писать: %+v", deps)
	}
}

// TestIngestDeploymentRecordFailure: сбой записи в реестр (здесь — ключ
// указывает на НЕсуществующий проект, FK deployments.project_id) отвечает 503,
// а не 200 с несохранённым деплоем: CI обязан увидеть отказ и повторить.
func TestIngestDeploymentRecordFailure(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)
	ghost := projectID + 1_000_000
	h.keys = NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: ghost, OrgID: 1, Kind: org.KindLegacy}})

	rec := deployRequest(h, ghost, `{"version":"v1.2.3"}`, true, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503, body=%s", rec.Code, rec.Body.String())
	}

	deps, err := h.Deploy.Recent(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("несохранённый деплой не должен появиться: %+v", deps)
	}
}
