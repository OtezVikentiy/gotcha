package ingest

import (
	"context"
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

	h = NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: projectID, OrgID: orgID}}), nil, nil, 1<<20)
	h.Deploy = deploy.NewStore(pool)
	return h, projectID
}

// postDeploy шлёт POST /api/{project}/deployments/ с sentry_key и телом body.
func postDeploy(t *testing.T, h *Handler, projectID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST",
		"/api/"+strconv.FormatInt(projectID, 10)+"/deployments/?sentry_key=deadbeef", strings.NewReader(body))
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

// TestIngestDeploymentDisabled: без сконфигурированного стора эндпоинт отвечает 503.
func TestIngestDeploymentDisabled(t *testing.T) {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1}}), nil, nil, 1<<20)
	rec := postDeploy(t, h, 1, `{"version":"v1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("h.Deploy==nil → %d, want 503", rec.Code)
	}
}
