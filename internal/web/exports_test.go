package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

const (
	exportsMaxActivePerUser = 3
	exportsCreateRateLimit  = 10
)

var okForm = url.Values{"kind": {"issues"}, "format": {"csv"}}

type exportsStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	h    *web.Handler
	org  *org.Service

	projectID      int64
	otherProjectID int64
	teamID         int64 // команда operatorUID, привязанная к projectID

	adminUID, operatorUID, viewerUID, otherUserUID         int64
	adminCookie, operatorCookie, viewerCookie, otherCookie *http.Cookie
}

func newExportsStack(t *testing.T) *exportsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query // страницы выгрузок событий не трогают

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Exports = export.NewStore(pool)
	h.ExportDir = t.TempDir()
	h.Register(mux)

	ctx := context.Background()
	register := func(email string) (int64, *http.Cookie) {
		uid, err := authSvc.Register(ctx, email, "correct-horse-battery")
		if err != nil {
			t.Fatalf("register %s: %v", email, err)
		}
		token, err := authSvc.CreateSession(ctx, uid)
		if err != nil {
			t.Fatalf("create session %s: %v", email, err)
		}
		return uid, &http.Cookie{Name: auth.CookieName, Value: token}
	}

	adminUID, adminCookie := register("exports-admin@example.com")
	operatorUID, operatorCookie := register("exports-operator@example.com")
	viewerUID, viewerCookie := register("exports-viewer@example.com")
	otherUserUID, otherCookie := register("exports-other@example.com")

	o, err := orgSvc.CreateOrg(ctx, "exports-co", "Exports Co", adminUID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "exports-proj", "Exports Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orgSvc.AddMember(ctx, o.ID, operatorUID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	team, err := orgSvc.CreateTeam(ctx, o.ID, "exports-team", "exports-team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, team.ID, operatorUID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	o2, err := orgSvc.CreateOrg(ctx, "exports-co-2", "Exports Co 2", otherUserUID)
	if err != nil {
		t.Fatalf("create org2: %v", err)
	}
	proj2, err := orgSvc.CreateProject(ctx, o2.ID, "exports-proj-2", "Exports Proj 2", "go")
	if err != nil {
		t.Fatalf("create project2: %v", err)
	}

	return &exportsStack{
		pool: pool, srv: srv, h: h, org: orgSvc,
		projectID: proj.ID, otherProjectID: proj2.ID, teamID: team.ID,
		adminUID: adminUID, operatorUID: operatorUID, viewerUID: viewerUID, otherUserUID: otherUserUID,
		adminCookie: adminCookie, operatorCookie: operatorCookie, viewerCookie: viewerCookie, otherCookie: otherCookie,
	}
}

func (s *exportsStack) cookie(t *testing.T, uid int64) *http.Cookie {
	t.Helper()
	switch uid {
	case s.adminUID:
		return s.adminCookie
	case s.operatorUID:
		return s.operatorCookie
	case s.viewerUID:
		return s.viewerCookie
	case s.otherUserUID:
		return s.otherCookie
	}
	t.Fatalf("exportsStack: неизвестный uid %d", uid)
	return nil
}

func (s *exportsStack) path(suffix string) string {
	return fmt.Sprintf("/projects/%d%s", s.projectID, suffix)
}

func (s *exportsStack) postForm(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	return s.postFormAs(t, s.operatorUID, path, form)
}

func (s *exportsStack) postFormAs(t *testing.T, uid int64, path string, form url.Values) *http.Response {
	t.Helper()
	return postForm(t, s.srv, path, form, s.srv.URL, s.cookie(t, uid))
}

func (s *exportsStack) getAs(t *testing.T, uid int64, path string) *http.Response {
	t.Helper()
	return getWithCookie(t, s.srv, path, s.cookie(t, uid))
}

func (s *exportsStack) getBody(t *testing.T, uid int64, path string) string {
	t.Helper()
	resp := s.getAs(t, uid, path)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
	}
	return body
}

func (s *exportsStack) lastJobID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM export_jobs ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("last job id: %v", err)
	}
	return id
}

func (s *exportsStack) lastJob(t *testing.T) export.Job {
	t.Helper()
	j, err := s.h.Exports.Get(context.Background(), s.lastJobID(t))
	if err != nil {
		t.Fatalf("get last job: %v", err)
	}
	return j
}

func (s *exportsStack) jobCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM export_jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func (s *exportsStack) markDone(t *testing.T, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status='done', finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`, id); err != nil {
		t.Fatalf("markDone: %v", err)
	}
}

func (s *exportsStack) markStatus(t *testing.T, id int64, status string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status=$2 WHERE id=$1`, id, status); err != nil {
		t.Fatalf("markStatus: %v", err)
	}
}

func (s *exportsStack) markFailed(t *testing.T, id int64, reasonKey string) {
	t.Helper()
	var arg any
	if reasonKey != "" {
		arg = reasonKey
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status='failed', finished_at=now(), last_error='техническая причина', failure_reason_key=$2 WHERE id=$1`,
		id, arg); err != nil {
		t.Fatalf("markFailed: %v", err)
	}
}

func (s *exportsStack) markDoneTruncated(t *testing.T, id int64, rows, bytes int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status='done', rows_written=$2, bytes=$3, truncated=true,
			finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`,
		id, rows, bytes); err != nil {
		t.Fatalf("markDoneTruncated: %v", err)
	}
}

func (s *exportsStack) enqueueAndFinish(t *testing.T, uid int64) {
	t.Helper()
	resp := s.postFormAs(t, uid, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enqueueAndFinish: POST = %d, want 303: %s", resp.StatusCode, body)
	}
	s.markDone(t, s.lastJobID(t))
}

func (s *exportsStack) enqueueAs(t *testing.T, uid int64) int64 {
	t.Helper()
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.otherProjectID,
		CreatedBy: uid,
		Kind:      export.KindIssues,
		Format:    export.FormatCSV,
		Params:    export.Params{Since: time.Now().Add(-24 * time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueueAs: %v", err)
	}
	path := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(path, []byte("id,title\n1,demo\n"), 0o644); err != nil {
		t.Fatalf("enqueueAs: write file: %v", err)
	}
	s.markDone(t, id)
	return id
}

func (s *exportsStack) enqueueDone(t *testing.T, uid int64) int64 {
	t.Helper()
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID,
		CreatedBy: uid,
		Kind:      export.KindIssues,
		Format:    export.FormatCSV,
		Params:    export.Params{Since: time.Now().Add(-24 * time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueueDone: enqueue: %v", err)
	}
	path := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(path, []byte("id,title\n1,demo\n"), 0o644); err != nil {
		t.Fatalf("enqueueDone: write file: %v", err)
	}
	s.markDone(t, id)
	return id
}

func (s *exportsStack) enqueueDoneWithoutFile(t *testing.T, uid int64) int64 {
	t.Helper()
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID,
		CreatedBy: uid,
		Kind:      export.KindIssues,
		Format:    export.FormatCSV,
		Params:    export.Params{Since: time.Now().Add(-24 * time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueueDoneWithoutFile: enqueue: %v", err)
	}
	s.markDone(t, id)
	return id
}

func (s *exportsStack) revokeProjectAccess(t *testing.T, uid int64) {
	t.Helper()
	if err := s.org.RemoveTeamMember(context.Background(), s.teamID, uid); err != nil {
		t.Fatalf("revoke access: %v", err)
	}
}

func TestExportsCreateFreezesRelativePeriod(t *testing.T) {
	s := newExportsStack(t)
	before := time.Now().UTC()

	resp := s.postForm(t, s.path("/exports?period=24h"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код ответа %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	j := s.lastJob(t)
	if j.Params.Since.IsZero() || j.Params.Until.IsZero() {
		t.Fatal("период не развёрнут в абсолютный — файл будет невоспроизводим")
	}
	if j.Params.Until.Before(before) {
		t.Errorf("верхняя граница %v раньше момента постановки %v", j.Params.Until, before)
	}
	if got := j.Params.Until.Sub(j.Params.Since); got < 23*time.Hour || got > 25*time.Hour {
		t.Errorf("окно = %v, ожидали ~24ч", got)
	}
}

func TestExportsCreateDefaultsToAllTimeWithoutPeriod(t *testing.T) {
	s := newExportsStack(t)

	resp := s.postForm(t, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код ответа %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	j := s.lastJob(t)
	if !j.Params.Since.IsZero() {
		t.Errorf("Since = %v, ожидали нулевое (без нижней границы)", j.Params.Since)
	}
	if !j.Params.Until.IsZero() {
		t.Errorf("Until = %v, ожидали нулевое (без верхней границы)", j.Params.Until)
	}
}

func TestExportsCreateIgnoresRangeCookieWithoutExplicitPeriod(t *testing.T) {
	s := newExportsStack(t)

	req, err := http.NewRequest(http.MethodPost, s.srv.URL+s.path("/exports"), strings.NewReader(okForm.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.srv.URL)
	req.AddCookie(s.cookie(t, s.operatorUID))
	req.AddCookie(&http.Cookie{Name: "range", Value: "24h"})

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код ответа %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	j := s.lastJob(t)
	if !j.Params.Since.IsZero() {
		t.Errorf("Since = %v, ожидали нулевое (без нижней границы) — период уехал из cookie rangeCookie=24h вместо RangeAll", j.Params.Since)
	}
	if !j.Params.Until.IsZero() {
		t.Errorf("Until = %v, ожидали нулевое (без верхней границы) — период уехал из cookie rangeCookie=24h вместо RangeAll", j.Params.Until)
	}
}

func TestExportsCreateHonorsExplicitPeriodQuery(t *testing.T) {
	s := newExportsStack(t)
	before := time.Now().UTC()

	resp := s.postForm(t, s.path("/exports?period=7d"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код ответа %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	j := s.lastJob(t)
	if j.Params.Since.IsZero() || j.Params.Until.IsZero() {
		t.Fatal("период не развёрнут в абсолютный")
	}
	if j.Params.Until.Before(before) {
		t.Errorf("верхняя граница %v раньше момента постановки %v", j.Params.Until, before)
	}
	if got := j.Params.Until.Sub(j.Params.Since); got < 6*24*time.Hour || got > 8*24*time.Hour {
		t.Errorf("окно = %v, ожидали ~7 суток", got)
	}
}

func TestExportsCreateHonorsCustomRangeQuery(t *testing.T) {
	s := newExportsStack(t)
	start := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Minute)
	end := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Minute)
	q := url.Values{
		"period": {"custom"},
		"start":  {start.Format("2006-01-02T15:04")},
		"end":    {end.Format("2006-01-02T15:04")},
	}

	resp := s.postForm(t, s.path("/exports?"+q.Encode()), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код ответа %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	j := s.lastJob(t)
	if !j.Params.Since.Equal(start) {
		t.Errorf("Since = %v, ожидали %v", j.Params.Since, start)
	}
	if !j.Params.Until.Equal(end) {
		t.Errorf("Until = %v, ожидали %v", j.Params.Until, end)
	}
}

func TestExportsCreateDeniedForNonOperator(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postFormAs(t, s.viewerUID, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404 (не 403): %s", resp.StatusCode, body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок вместо нуля", n)
	}
}

func TestExportsCreateIgnoresPIIFlagFromOperator(t *testing.T) {
	s := newExportsStack(t)

	resp := s.postFormAs(t, s.operatorUID, s.path("/exports"),
		url.Values{"kind": {"events"}, "format": {"json"}, "include_pii": {"on"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("оператор: код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	if s.lastJob(t).IncludePII {
		t.Fatal("оператор выпросил выгрузку без маски")
	}

	resp = s.postFormAs(t, s.adminUID, s.path("/exports"),
		url.Values{"kind": {"events"}, "format": {"json"}, "include_pii": {"on"}})
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("админ: код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	if !s.lastJob(t).IncludePII {
		t.Fatal("админу галка не сработала")
	}
}

func TestExportsCreateRefusesOverActiveLimit(t *testing.T) {
	s := newExportsStack(t)
	for i := 0; i < exportsMaxActivePerUser; i++ {
		resp := s.postForm(t, s.path("/exports"), okForm)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("заявка %d: код %d, ожидали редирект: %s", i, resp.StatusCode, body)
		}
	}
	resp := s.postForm(t, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при превышении лимита активных заявок: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, `class="error-code"`) {
		t.Errorf("отказ ушёл на chromeless ErrorPage вместо перерисовки страницы «Выгрузки»: %s", body)
	}
	if !strings.Contains(body, `name="kind"`) {
		t.Errorf("форма постановки заявки не отрисована при отказе: %s", body)
	}
	if !strings.Contains(body, "Достигнут лимит одновременных заявок") {
		t.Errorf("сообщение об ошибке лимита не показано на странице: %s", body)
	}
}

func TestExportsCreateRateLimited(t *testing.T) {
	s := newExportsStack(t)
	for i := 0; i < exportsCreateRateLimit; i++ {
		s.enqueueAndFinish(t, s.operatorUID)
	}
	resp := s.postForm(t, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидали 429 после %d заявок подряд: %s", resp.StatusCode, exportsCreateRateLimit, body)
	}
	if strings.Contains(body, `class="error-code"`) {
		t.Errorf("отказ ушёл на chromeless ErrorPage вместо перерисовки страницы «Выгрузки»: %s", body)
	}
	if !strings.Contains(body, "Слишком много заявок на выгрузку подряд") {
		t.Errorf("сообщение об ошибке лимита частоты не показано на странице: %s", body)
	}
}

func TestExportsCreateInvalidKindReRendersExportsPageWithFormState(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{"kind": {"bogus"}, "format": {"ndjson"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при неизвестном kind, ожидали 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Неизвестный вид выгрузки") {
		t.Errorf("сообщение об ошибке не показано: %s", body)
	}
	if !strings.Contains(body, `<option value="ndjson" selected>`) {
		t.Errorf("введённый формат (ndjson) не восстановлен в форме: %s", body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок при невалидном kind вместо нуля", n)
	}
}

func TestExportsCreateInvalidFormatReRendersExportsPage(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{"kind": {"events"}, "format": {"bogus"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при неизвестном format, ожидали 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Неизвестный формат выгрузки") {
		t.Errorf("сообщение об ошибке не показано: %s", body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок при невалидном format вместо нуля", n)
	}
}

func TestExportsCreateRejectsInvalidStatus(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"issues"}, "format": {"csv"}, "status": {"bogus"},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при неизвестном status, ожидали 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Неизвестный статус в фильтре выгрузки") {
		t.Errorf("точное сообщение об ошибке status не показано: %s", body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок при невалидном status вместо нуля", n)
	}
}

func TestExportsCreateRejectsInvalidLevel(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"issues"}, "format": {"csv"}, "level": {"bogus"},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при неизвестном level, ожидали 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Неизвестный уровень в фильтре выгрузки") {
		t.Errorf("точное сообщение об ошибке level не показано: %s", body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок при невалидном level вместо нуля", n)
	}
}

func TestExportsCreateAcceptsKnownStatusAndLevel(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"issues"}, "format": {"csv"}, "status": {"resolved"}, "level": {"error"},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	job := s.lastJob(t)
	if job.Params.Status != "resolved" {
		t.Errorf("Params.Status = %q, want %q", job.Params.Status, "resolved")
	}
	if job.Params.Level != "error" {
		t.Errorf("Params.Level = %q, want %q", job.Params.Level, "error")
	}
}

func TestExportsCreateAcceptsEmptyStatusAndLevel(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{"kind": {"issues"}, "format": {"csv"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код %d при пустых status/level, ожидали редирект: %s", resp.StatusCode, body)
	}
	job := s.lastJob(t)
	if job.Params.Status != "" {
		t.Errorf("Params.Status = %q, want \"\" (любой)", job.Params.Status)
	}
	if job.Params.Level != "" {
		t.Errorf("Params.Level = %q, want \"\" (любой)", job.Params.Level)
	}
}

func TestExportsCreateRejectsForeignScopeIssueId(t *testing.T) {
	s := newExportsStack(t)
	res, err := s.h.Issues.Upsert(context.Background(), s.otherProjectID, "fp-foreign", "Foreign", "pkg/a.go:1", "error", "", time.Now())
	if err != nil {
		t.Fatalf("upsert foreign issue: %v", err)
	}

	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"issues"}, "format": {"csv"}, "scope_issue_id": {strconv.FormatInt(res.IssueID, 10)},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при scope_issue_id из чужого проекта, ожидали 422: %s", resp.StatusCode, body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок вместо нуля — холостая заявка сожгла слот лимита", n)
	}
	if !strings.Contains(body, "Указанная проблема не найдена в этом проекте") {
		t.Errorf("нет сообщения об ошибке про чужую/несуществующую группу: %s", body)
	}
}

func TestExportsCreateRejectsUnknownScopeIssueId(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"issues"}, "format": {"csv"}, "scope_issue_id": {"999999999"},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при несуществующем scope_issue_id, ожидали 422: %s", resp.StatusCode, body)
	}
	if n := s.jobCount(t); n != 0 {
		t.Errorf("создано %d заявок вместо нуля", n)
	}
}

func TestExportsCreateAcceptsOwnScopeIssueId(t *testing.T) {
	s := newExportsStack(t)
	res, err := s.h.Issues.Upsert(context.Background(), s.projectID, "fp-own", "Own", "pkg/a.go:1", "error", "", time.Now())
	if err != nil {
		t.Fatalf("upsert own issue: %v", err)
	}

	resp := s.postForm(t, s.path("/exports"), url.Values{
		"kind": {"events"}, "format": {"csv"}, "scope_issue_id": {strconv.FormatInt(res.IssueID, 10)},
	})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	if got := s.lastJob(t).ScopeIssueID; got != res.IssueID {
		t.Errorf("ScopeIssueID заявки = %d, want %d", got, res.IssueID)
	}
}

func TestExportsDownloadForeignJobIs404(t *testing.T) {
	s := newExportsStack(t)
	other := s.enqueueAs(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", other)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("заявка из чужого проекта отдана с кодом %d: %s", resp.StatusCode, body)
	}
}

func TestExportsDeleteForeignJobIs404(t *testing.T) {
	s := newExportsStack(t)
	other := s.enqueueAs(t, s.operatorUID)

	resp := s.postFormAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/delete", other)), url.Values{})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("заявка из чужого проекта удалена с кодом %d: %s", resp.StatusCode, body)
	}
	if _, err := s.h.Exports.Get(context.Background(), other); err != nil {
		t.Fatalf("заявка из чужого проекта пропала после отказа в доступе (ожидали, что она цела): %v", err)
	}
}

func TestExportsDownloadRechecksProjectAccess(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDone(t, s.operatorUID)
	s.revokeProjectAccess(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("файл отдан пользователю без доступа к проекту: код %d: %s", resp.StatusCode, body)
	}
}

func TestExportsDownloadSetsAttachmentHeaders(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDone(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d, ожидали 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, ".csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if body != "id,title\n1,demo\n" {
		t.Errorf("тело файла = %q, не совпадает с записанным на диск", body)
	}
}

func TestExportsDeleteRejectsRunningJob(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), okForm)
	readAll(t, resp)
	id := s.lastJobID(t)

	resp = s.postForm(t, s.path(fmt.Sprintf("/exports/%d/delete", id)), url.Values{})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("код %d при удалении queued-заявки, ожидали 422: %s", resp.StatusCode, body)
	}
	if _, err := s.h.Exports.Get(context.Background(), id); err != nil {
		t.Fatalf("queued-заявка пропала: %v", err)
	}
}

func TestExportsDeleteRemovesFileThenRow(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDone(t, s.operatorUID)
	filePath := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))

	resp := s.postForm(t, s.path(fmt.Sprintf("/exports/%d/delete", id)), url.Values{"confirmed": {"yes"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("файл %s не удалён: err=%v", filePath, err)
	}
	if _, err := s.h.Exports.Get(context.Background(), id); !errors.Is(err, export.ErrNotFound) {
		t.Errorf("строка заявки не удалена: err=%v", err)
	}
}

func TestExportsDeleteRequiresConfirmation(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDone(t, s.operatorUID)
	filePath := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))

	resp := s.postForm(t, s.path(fmt.Sprintf("/exports/%d/delete", id)), url.Values{})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("без confirmed=yes: код %d, ожидали 200 (страница подтверждения): %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `action="`+s.path(fmt.Sprintf("/exports/%d/delete", id))+`"`) {
		t.Errorf("страница подтверждения не ведёт на тот же action: %s", body)
	}
	if !strings.Contains(body, `name="confirmed" value="yes"`) {
		t.Errorf("на странице подтверждения нет скрытого поля confirmed=yes: %s", body)
	}
	if !strings.Contains(body, "Группы проблем") {
		t.Errorf("страница подтверждения не называет вид выгружаемых данных: %s", body)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("файл снесён ДО подтверждения: %v", err)
	}
	if _, err := s.h.Exports.Get(context.Background(), id); err != nil {
		t.Fatalf("строка заявки снесена ДО подтверждения: %v", err)
	}

	resp = s.postForm(t, s.path(fmt.Sprintf("/exports/%d/delete", id)), url.Values{"confirmed": {"yes"}})
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("с confirmed=yes: код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("файл не удалён после подтверждения: err=%v", err)
	}
	if _, err := s.h.Exports.Get(context.Background(), id); !errors.Is(err, export.ErrNotFound) {
		t.Errorf("строка заявки не удалена после подтверждения: err=%v", err)
	}
}

func TestExportsRoutesDisabledWhenExportsNil(t *testing.T) {
	s := newExportsStack(t)
	s.h.Exports = nil

	if resp := s.postForm(t, s.path("/exports"), okForm); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /exports = %d, want 404", resp.StatusCode)
	}
	if resp := s.getAs(t, s.operatorUID, s.path("/exports/1/download")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET download = %d, want 404", resp.StatusCode)
	}
	if resp := s.postForm(t, s.path("/exports/1/delete"), url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST delete = %d, want 404", resp.StatusCode)
	}
}

func TestExportsPageShowsExplanationWhenExportsNil(t *testing.T) {
	s := newExportsStack(t)
	s.h.Exports = nil

	resp := s.getAs(t, s.operatorUID, s.path("/exports"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /exports = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Выгрузки выключены на этом инстансе") {
		t.Errorf("нет объяснения о выключенной фиче: %s", body)
	}
	if strings.Contains(body, "data-table") {
		t.Error("таблица заявок отрисована при выключенной фиче")
	}
	if strings.Contains(body, `name="kind"`) {
		t.Error("форма постановки заявки отрисована при выключенной фиче")
	}

	if resp := s.getAs(t, s.viewerUID, s.path("/exports")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /exports (не-оператор) = %d, want 404", resp.StatusCode)
	}
}

func TestExportsDownloadInvalidJobIDIs404(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.operatorUID, s.path("/exports/not-a-number/download"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

func TestExportsDeleteInvalidJobIDIs404(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports/not-a-number/delete"), url.Values{})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

func TestExportsDownloadMissingFileIs404(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDoneWithoutFile(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

func TestExportsPageShowsStatusesAndTruncation(t *testing.T) {
	s := newExportsStack(t)
	runningID, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindIssues, Format: export.FormatCSV,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue running: %v", err)
	}
	s.markStatus(t, runningID, "running")

	truncatedID, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindEvents, Format: export.FormatNDJSON,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue truncated: %v", err)
	}
	s.markDoneTruncated(t, truncatedID, 1000, 2048)

	body := s.getBody(t, s.operatorUID, s.path("/exports"))
	for _, want := range []string{"Выгрузки", "выполняется", "обрезана"} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
}

func TestExportsPageShowsFailureReasonHint(t *testing.T) {
	s := newExportsStack(t)
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindIssues, Format: export.FormatCSV,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	s.markFailed(t, id, "exports.mail.failed.reason.disk_full")

	body := s.getBody(t, s.operatorUID, s.path("/exports"))
	if !strings.Contains(body, "на диске выгрузок закончилось место") {
		t.Errorf("на странице нет переведённой причины отказа: %s", body)
	}
	if strings.Contains(body, "техническая причина") {
		t.Error("техтекст last_error утёк на страницу")
	}
}

func TestExportsPageHidesReasonHintForUnknownOrMissingKey(t *testing.T) {
	s := newExportsStack(t)
	legacyID, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindIssues, Format: export.FormatCSV,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue legacy: %v", err)
	}
	s.markFailed(t, legacyID, "") // NULL — строка старше миграции

	unknownID, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindIssues, Format: export.FormatCSV,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue unknown: %v", err)
	}
	s.markFailed(t, unknownID, "some.corrupted.key")

	body := s.getBody(t, s.operatorUID, s.path("/exports"))
	if strings.Contains(body, "some.corrupted.key") {
		t.Errorf("неизвестный ключ утёк на страницу как есть: %s", body)
	}
}

func TestExportsPageHidesDeleteForRunningJob(t *testing.T) {
	s := newExportsStack(t)
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID, Kind: export.KindIssues, Format: export.FormatCSV,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	s.markStatus(t, id, "running")

	body := s.getBody(t, s.operatorUID, s.path("/exports"))
	if strings.Contains(body, fmt.Sprintf("/exports/%d/delete", id)) {
		t.Error("кнопка удаления показана для выполняющейся заявки")
	}
}

func TestExportsPageEmptyState(t *testing.T) {
	s := newExportsStack(t)
	body := s.getBody(t, s.operatorUID, s.path("/exports"))
	if !strings.Contains(body, "Заявок ещё нет") {
		t.Error("нет пустого состояния списка заявок")
	}
	if strings.Contains(body, "data-table") {
		t.Error("таблица отрисована при пустом списке заявок")
	}
}

func TestIssuesPageHasExportButtons(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, ownerCookie := registerAndLogin(t, s, "issues-export-btn@example.com")
	project := createProject(t, s, ownerID, "export-btn-org", "export-btn-proj")
	s.h.Exports = export.NewStore(s.pool)
	t.Cleanup(func() { s.h.Exports = nil })
	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-export-btn", "Boom", "pkg/a.go:1", "error", "", time.Now()); err != nil {
		t.Fatalf("upsert issue: %v", err)
	}
	pid := strconv.FormatInt(project.ID, 10)
	body := readAll(t, getWithCookie(t, s.srv, "/projects/"+pid+"/issues", ownerCookie))
	if !strings.Contains(body, `action="/projects/`+pid+`/exports?period=all"`) {
		t.Error("на списке ошибок нет формы выгрузки с переносом периода (?period=all)")
	}
}

func TestExportsPagePIIHintPresent(t *testing.T) {
	s := newExportsStack(t)
	body := s.getBody(t, s.adminUID, s.path("/exports"))
	if !strings.Contains(body, "Маскирование необратимо") {
		t.Error("нет подсказки про скрабинг на приёме")
	}
}

func TestExportsPageIgnoresLimitQueryParam(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.operatorUID, s.path("/exports?limit=-1"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d при ?limit=-1, ожидали 200 (limit — константа пакета): %s", resp.StatusCode, body)
	}
}

func TestExportsPageDeniedForNonOperator(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.viewerUID, s.path("/exports"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404 (не 403): %s", resp.StatusCode, body)
	}
}

func TestExportsPageHidesForeignJobWhenNotManager(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postFormAs(t, s.adminUID, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("постановка админом: код %d, ожидали редирект: %s", resp.StatusCode, body)
	}
	adminJobID := s.lastJobID(t)
	s.markDone(t, adminJobID)

	operatorView := s.getBody(t, s.operatorUID, s.path("/exports"))
	if strings.Contains(operatorView, fmt.Sprintf("/exports/%d/download", adminJobID)) {
		t.Error("оператор без CanManage видит скачивание чужой заявки")
	}
	if strings.Contains(operatorView, fmt.Sprintf("/exports/%d/delete", adminJobID)) {
		t.Error("оператор без CanManage видит удаление чужой заявки")
	}
	if strings.Contains(operatorView, "exports-admin@example.com") {
		t.Error("оператор без CanManage видит email автора чужой заявки — строка не должна рендериться вовсе")
	}

	adminView := s.getBody(t, s.adminUID, s.path("/exports"))
	if !strings.Contains(adminView, fmt.Sprintf("/exports/%d/download", adminJobID)) {
		t.Error("админ (CanManage) не видит скачивание своей же заявки")
	}
	if !strings.Contains(adminView, fmt.Sprintf("/exports/%d/delete", adminJobID)) {
		t.Error("админ (CanManage) не видит удаление своей же заявки")
	}
}

func TestExportsPageBatchesAuthorEmailsAcrossDistinctAuthors(t *testing.T) {
	s := newExportsStack(t)
	adminJobID := s.enqueueDone(t, s.adminUID)
	operatorJobID := s.enqueueDone(t, s.operatorUID)

	body := s.getBody(t, s.adminUID, s.path("/exports"))

	adminRow := rowContaining(t, body, fmt.Sprintf("/exports/%d/download", adminJobID))
	if !strings.Contains(adminRow, "exports-admin@example.com") {
		t.Errorf("строка заявки %d (автор admin) не содержит email админа: %s", adminJobID, adminRow)
	}
	if strings.Contains(adminRow, "exports-operator@example.com") {
		t.Errorf("строка заявки %d (автор admin) содержит email оператора — email перепутан между строками: %s", adminJobID, adminRow)
	}

	operatorRow := rowContaining(t, body, fmt.Sprintf("/exports/%d/download", operatorJobID))
	if !strings.Contains(operatorRow, "exports-operator@example.com") {
		t.Errorf("строка заявки %d (автор operator) не содержит email оператора: %s", operatorJobID, operatorRow)
	}
	if strings.Contains(operatorRow, "exports-admin@example.com") {
		t.Errorf("строка заявки %d (автор operator) содержит email админа — email перепутан между строками: %s", operatorJobID, operatorRow)
	}
}

func rowContaining(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("маркер %q не найден на странице: %s", marker, body)
	}
	start := strings.LastIndex(body[:i], "<tr>")
	end := strings.Index(body[i:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("не удалось выделить <tr> вокруг маркера %q", marker)
	}
	return body[start : i+end]
}

func TestExportIssueURLHitsRegisteredRoute(t *testing.T) {
	s := newStack(t)

	const base = "https://gotcha.example.com"
	got := export.IssueURL(base, 1049)
	path, ok := strings.CutPrefix(got, base)
	if !ok {
		t.Fatalf("IssueURL = %q, want абсолютную ссылку с префиксом %q", got, base)
	}
	assertRouteRegistered(t, s, http.MethodGet, path)
}

func TestExportsMetaEndpointReturnsBuildMeta(t *testing.T) {
	s := newExportsStack(t)
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID,
		Kind: export.KindEvents, Format: export.FormatNDJSON, IncludePII: false,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	s.markDone(t, id)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download?meta=1", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d, ожидали 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var meta export.Meta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("json.Unmarshal: %v (%q)", err, body)
	}
	if meta.ScopeIssueID != 0 {
		t.Errorf("ScopeIssueID = %d, want 0", meta.ScopeIssueID)
	}
	if meta.FilterCode != export.FilterCodeAll {
		t.Errorf("FilterCode = %q, want %q", meta.FilterCode, export.FilterCodeAll)
	}
	if meta.PseudonymNote != export.PseudonymUniquenessNote {
		t.Errorf("PseudonymNote = %q, want %q", meta.PseudonymNote, export.PseudonymUniquenessNote)
	}
}

func TestExportsMetaEndpointScopeIssueID(t *testing.T) {
	s := newExportsStack(t)
	id, err := s.h.Exports.Enqueue(context.Background(), export.Job{
		ProjectID: s.projectID, CreatedBy: s.operatorUID,
		Kind: export.KindIssues, Format: export.FormatCSV, ScopeIssueID: 99,
		Params: export.Params{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	s.markDone(t, id)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download?meta=1", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d, ожидали 200: %s", resp.StatusCode, body)
	}
	var meta export.Meta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("json.Unmarshal: %v (%q)", err, body)
	}
	if meta.ScopeIssueID != 99 {
		t.Errorf("ScopeIssueID = %d, want 99", meta.ScopeIssueID)
	}
	if meta.FilterCode != export.FilterCodeIssue {
		t.Errorf("FilterCode = %q, want %q", meta.FilterCode, export.FilterCodeIssue)
	}
	if meta.PseudonymNote != "" {
		t.Errorf("PseudonymNote = %q, want пусто (kind=issues)", meta.PseudonymNote)
	}
}

func TestExportsMetaEndpointForeignJobIs404(t *testing.T) {
	s := newExportsStack(t)
	other := s.enqueueAs(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download?meta=1", other)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("метаданные чужой заявки отданы с кодом %d: %s", resp.StatusCode, body)
	}
}

func TestExportsMetaEndpointNotDoneIs404(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports"), okForm)
	readAll(t, resp)
	id := s.lastJobID(t)

	resp = s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download?meta=1", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("метаданные queued-заявки отданы с кодом %d: %s", resp.StatusCode, body)
	}
}
