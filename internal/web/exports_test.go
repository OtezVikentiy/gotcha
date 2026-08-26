package web_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

// Зеркала неэкспортируемых лимитов internal/web/exports.go — та же техника,
// что и emailLimitPerWindow в register_invite_test.go: тест внешнего пакета
// web_test не видит maxActivePerUser/createRateLimit напрямую, значения
// обязаны совпадать с продовыми (см. exports.go).
const (
	exportsMaxActivePerUser = 3
	exportsCreateRateLimit  = 10
)

// okForm — минимальная валидная форма постановки заявки.
var okForm = url.Values{"kind": {"issues"}, "format": {"csv"}}

// exportsStack — стенд задачи 10: организация с четырьмя ролями и ВТОРОЙ,
// не связанный с первым, проект — нужен для проверки межпроектной изоляции
// Store.Get/Delete (принимают только id, без projectID).
//
//   - adminUID — владелец организации (CanManage=true), «admin» брифа;
//   - operatorUID — доступ к проекту ТОЛЬКО через команду (CanManage=false),
//     тот же приём, что TestRequireProjectOperatorReturnsAuthz (operate_test.go);
//   - viewerUID — не состоит ни в организации, ни в команде: доступа к
//     проекту нет вовсе (canOperateProject==CanAccessProject, operate.go) —
//     тот же existence-oracle, что и у остальных lvlOperator-страниц: 404;
//   - otherUserUID — владелец ВТОРОГО проекта (otherProjectID), заявки
//     которого используются как «чужие» для projectID.
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
	// viewerUID сознательно не добавляется никуда — у него нет доступа к
	// проекту вовсе.

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

// postForm — POST от имени operatorUID (дефолтный актор большинства
// сценариев постановки заявки).
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

// getBody — GET, ожидающий 200, тело для strings.Contains-проверок разметки
// (страница «Выгрузки» и её кнопки на issues/issuedetail).
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

// markDone переводит заявку в done напрямую через SQL — тесты этого файла
// обходят воркер (задача 8, отдельная), им нужен только терминальный
// статус, а не реальная сборка файла.
func (s *exportsStack) markDone(t *testing.T, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status='done', finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`, id); err != nil {
		t.Fatalf("markDone: %v", err)
	}
}

// markStatus — выставляет статус заявки напрямую через SQL, в обход
// Store.Claim/Done/Fail (которые фенсят под status='running' AND attempts=N)
// — странице списка (задача 11) нужен только конечный статус на экране, а
// не реальный цикл воркера (тот же приём, что markDone выше).
func (s *exportsStack) markStatus(t *testing.T, id int64, status string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status=$2 WHERE id=$1`, id, status); err != nil {
		t.Fatalf("markStatus: %v", err)
	}
}

// markDoneTruncated — как markDone, но с truncated=true и ненулевыми
// rows/bytes — для проверки отметки «обрезана» на странице списка.
func (s *exportsStack) markDoneTruncated(t *testing.T, id int64, rows, bytes int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE export_jobs SET status='done', rows_written=$2, bytes=$3, truncated=true,
			finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`,
		id, rows, bytes); err != nil {
		t.Fatalf("markDoneTruncated: %v", err)
	}
}

// enqueueAndFinish — ставит заявку ЧЕРЕЗ HTTP (чтобы её засчитал лимитер
// частоты h.exportLimiter) и сразу освобождает слот активных заявок,
// переводя её в done: TestExportsCreateRateLimited проверяет ИМЕННО лимитер
// частоты, а не лимит активных заявок (exportsMaxActivePerUser=3), поэтому
// каждая итерация обязана заканчивать за собой заявку.
func (s *exportsStack) enqueueAndFinish(t *testing.T, uid int64) {
	t.Helper()
	resp := s.postFormAs(t, uid, s.path("/exports"), okForm)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enqueueAndFinish: POST = %d, want 303: %s", resp.StatusCode, body)
	}
	s.markDone(t, s.lastJobID(t))
}

// enqueueAs — заводит заявку НАПРЯМУЮ через Store в otherProjectID (uid —
// его владелец): используется как «чужая» заявка для projectID в тестах
// межпроектной изоляции. HTTP тут не годится — uid не оператор projectID.
// Заявка сразу помечается done: без этого TestExportsDownloadForeignJobIs404
// проверял бы совсем не то, что заявлено в его докблоке — запрос отсекался
// бы веткой job.Status != StatusDone (queued по умолчанию) РАНЬШЕ, чем
// дошёл бы до сверки job.ProjectID, и мутация, ломающая именно эту сверку,
// осталась бы незамеченной.
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
	// done + реальный файл на диске: единственная ветка, которая должна
	// отсекать межпроектный доступ, — сверка job.ProjectID. Не-done или
	// отсутствующий файл дали бы 404 по другой причине и замаскировали бы
	// мутацию, ломающую именно эту сверку (см. докблок
	// TestExportsDownloadForeignJobIs404).
	path := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(path, []byte("id,title\n1,demo\n"), 0o644); err != nil {
		t.Fatalf("enqueueAs: write file: %v", err)
	}
	s.markDone(t, id)
	return id
}

// enqueueDone — заявка uid в projectID, сразу done, С реальным файлом на
// диске (h.ExportDir/<id>.csv) — download открывает именно этот путь.
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

// enqueueDoneWithoutFile — заявка done, но БЕЗ файла на диске: имитирует
// расхождение строки и файла (джанитор не туда смотрел, файл снесли руками) —
// exportsDownload обязана отказать 404 и залогировать предупреждение, а не
// упасть при os.Open.
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

// TestExportsCreateFreezesRelativePeriod — заявка «за последние 24 часа»,
// исполненная позже, обязана дать тот же файл: период разворачивается в
// абсолютные границы В МОМЕНТ постановки, а не исполнения.
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

// TestExportsCreateDefaultsToAllTimeWithoutPeriod — POST без query-параметра
// периода (кнопки на issues/issuedetail до правки слали именно так) обязан
// ставить заявку «за всё время», а не молча урезать её до 24ч: список issues,
// с которого ставится большинство заявок, сам по умолчанию живёт на «за всё
// время» (RangeAll) и никогда не пишет "all" в rangeCookie. Мутация: вернуть
// дефолт resolveTimeRange в exportsCreate на "24h" — тест обязан упасть.
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

// TestExportsCreateHonorsExplicitPeriodQuery — период, переданный
// query-параметром на action формы (exportsPathWithRange), обязан дойти до
// Params: это то, что теперь реально шлют формы issues/issuedetail вместо
// бесполезного hidden-поля. Мутация: убрать period из action формы (то есть
// не передать его в запросе) — тест обязан упасть, откатившись на дефолт
// «за всё время» из предыдущего теста.
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

// TestExportsCreateHonorsCustomRangeQuery — произвольный диапазон, который
// TimeRangeVM.apply переносит в поля "start"/"end" query (не "cstart"/"cend"
// — те лишь для переноса custom между страницами при смене прочих фильтров),
// обязан дойти до Params ровно теми границами, что были заданы.
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

// TestExportsCreateDeniedForNonOperator — участник без доступа к проекту не
// выгружает: массовый вынос данных не должен быть доступен кому попало.
// 404, не 403 — существование проекта не раскрываем (тот же принцип, что у
// остальных lvlOperator-страниц).
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

// TestExportsCreateIgnoresPIIFlagFromOperator — галку «как есть» ставит
// только админ/владелец орга; оператору она молча игнорируется (не отказ —
// маска безопасна по умолчанию).
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

// TestExportsCreateRefusesOverActiveLimit — лимит активных (queued+running)
// заявок на пользователя (exportsMaxActivePerUser).
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
}

// TestExportsCreateRateLimited — лимит активных заявок не ловит того, кто
// ставит и тут же удаляет (завершает): тяжёлую выборку по ClickHouse
// защищает именно ограничение частоты.
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
}

// TestExportsDownloadForeignJobIs404 — Store.Get принимает только id, без
// projectID: заявка из ДРУГОГО проекта не должна отдаваться по
// /projects/{projectID}/exports/{jobID}/download только потому, что jobID
// совпал. Заявка заведена НА ТОГО ЖЕ operatorUID, что её запрашивает —
// авторская проверка (job.CreatedBy == uid) сама по себе прошла бы, изолируя
// проверку именно ProjectID: без неё тест не отличил бы отсутствующую
// сверку проекта от работающей сверки авторства.
func TestExportsDownloadForeignJobIs404(t *testing.T) {
	s := newExportsStack(t)
	other := s.enqueueAs(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", other)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("заявка из чужого проекта отдана с кодом %d: %s", resp.StatusCode, body)
	}
}

// TestExportsDeleteForeignJobIs404 — тот же инвариант, что и
// TestExportsDownloadForeignJobIs404, для удаления: без сверки job.ProjectID
// с {id} из маршрута Delete снёс бы заявку из другого проекта по одному
// jobID. Тот же приём изоляции: заявка заведена на самого operatorUID.
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

// TestExportsDownloadRechecksProjectAccess — доступ мог быть отозван ПОСЛЕ
// постановки заявки: авторства недостаточно, скачивание обязано
// перепроверить доступ к проекту в момент запроса.
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

// TestExportsDownloadSetsAttachmentHeaders — Content-Type из Format, имя
// файла с расширением в Content-Disposition: attachment.
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

// TestExportsDeleteRejectsRunningJob — удаление разрешено только для
// терминальных статусов: у queued/running в этот момент может писаться
// файл, а не только строка.
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

// TestExportsDeleteRemovesFileThenRow — успешное удаление сносит и файл на
// диске, и строку в таблице.
func TestExportsDeleteRemovesFileThenRow(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDone(t, s.operatorUID)
	filePath := filepath.Join(s.h.ExportDir, fmt.Sprintf("%d.csv", id))

	resp := s.postForm(t, s.path(fmt.Sprintf("/exports/%d/delete", id)), url.Values{})
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

// TestExportsRoutesDisabledWhenExportsNil — h.Exports == nil (выгрузки
// выключены на инстансе) обязан давать 404 на create/download/delete —
// это не UI, объяснять там нечего (ревью веб-части E1, п.3). Страница
// списка (GET /projects/{id}/exports) — исключение: спека E1 §10 требует
// на ней объяснение вместо пустой таблицы, а не 404, см.
// TestExportsPageShowsExplanationWhenExportsNil ниже.
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

// TestExportsPageShowsExplanationWhenExportsNil — h.Exports == nil обязан
// давать 200 с объяснением на странице списка, а не 404 и не пустую
// таблицу с рабочей формой постановки (спека E1 §10; ревью веб-части, п.3).
// Доступ по-прежнему только оператору проекта — не-оператор получает
// обычный 404 (TestExportsPageDeniedForNonOperator покрывает общий случай,
// здесь — что порядок гейтов не сломан при отключённой фиче).
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

// TestExportsDownloadInvalidJobIDIs404 — нечисловой jobID в пути обязан
// давать 404, а не паниковать/500. Проверяет только код ответа: конкретная
// ветка не различима снаружи и не гарантирована этим тестом — strconv.ParseInt
// отсекает такой jobID сама, но даже без этой проверки jobID=0 ушёл бы в
// Store.Get(0) и получил бы тот же 404 через ErrNotFound (двух путей к
// одному ответу достаточно, различать их снаружи незачем).
func TestExportsDownloadInvalidJobIDIs404(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.operatorUID, s.path("/exports/not-a-number/download"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

// TestExportsDeleteInvalidJobIDIs404 — тот же разбор пути, что и у download,
// в exportsDelete.
func TestExportsDeleteInvalidJobIDIs404(t *testing.T) {
	s := newExportsStack(t)
	resp := s.postForm(t, s.path("/exports/not-a-number/delete"), url.Values{})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

// TestExportsDownloadMissingFileIs404 — строка done есть, файла на диске
// нет (расхождение с уборкой/ручным вмешательством): 404, не 500 и не паника
// на os.Open.
func TestExportsDownloadMissingFileIs404(t *testing.T) {
	s := newExportsStack(t)
	id := s.enqueueDoneWithoutFile(t, s.operatorUID)

	resp := s.getAs(t, s.operatorUID, s.path(fmt.Sprintf("/exports/%d/download", id)))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404: %s", resp.StatusCode, body)
	}
}

// --- Задача 11: страница «Выгрузки», кнопки и i18n ---

// TestExportsPageShowsStatusesAndTruncation — заголовок, перевод статуса
// running и отметка «обрезана» — на экране (см. exports.templ:exportRow).
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

// TestExportsPageHidesDeleteForRunningJob — у queued/running кнопки удаления
// быть не должно: она разъедется с воркером, который ещё пишет файл.
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

// TestExportsPageEmptyState — пустой список объясняет, что делать, а не
// рисует голую таблицу без строк.
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

// TestIssuesPageHasExportButtons — на списке ошибок есть форма постановки
// заявки (кнопки экспорта групп/событий с текущими фильтрами).
func TestIssuesPageHasExportButtons(t *testing.T) {
	s := newExportsStack(t)
	body := s.getBody(t, s.operatorUID, s.path("/issues"))
	if !strings.Contains(body, `action="`+s.path("/exports")+`"`) {
		t.Error("на списке ошибок нет формы выгрузки")
	}
}

// TestExportsPagePIIHintPresent — подсказка про необратимость маскирования
// PII обязана быть видна тому, кому вообще доступна галка include_pii:
// иначе админ примет занулённые на приёме адреса за баг.
func TestExportsPagePIIHintPresent(t *testing.T) {
	s := newExportsStack(t)
	body := s.getBody(t, s.adminUID, s.path("/exports"))
	if !strings.Contains(body, "Маскирование необратимо") {
		t.Error("нет подсказки про скрабинг на приёме")
	}
}

// TestExportsPageIgnoresLimitQueryParam — Store.ByProject НЕ санитизирует
// limit (отрицательное значение — сырая ошибка PostgreSQL, см. её докблок);
// exportsPage обязана звать её с фиксированной exportsListLimit, а не со
// значением из query. ?limit=-1, доехавший до SQL, дал бы 500 — мутация,
// подменяющая константу на query-параметр, обязана уронить этот тест.
func TestExportsPageIgnoresLimitQueryParam(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.operatorUID, s.path("/exports?limit=-1"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d при ?limit=-1, ожидали 200 (limit — константа пакета): %s", resp.StatusCode, body)
	}
}

// TestExportsPageDeniedForNonOperator — тот же гейт, что у create/download/
// delete: массовый список заявок проекта не должен быть виден кому попало.
func TestExportsPageDeniedForNonOperator(t *testing.T) {
	s := newExportsStack(t)
	resp := s.getAs(t, s.viewerUID, s.path("/exports"))
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("код %d, ожидали 404 (не 403): %s", resp.StatusCode, body)
	}
}

// TestExportsPageHidesForeignJobWhenNotManager — оператор без CanManage не
// должен видеть чужую (не свою) заявку на странице выгрузок вовсе — ни
// строки, ни email автора, ни колонки PII (спека §3: «свои заявки видит
// автор; админ и владелец орга видят все заявки проекта» — эта строка несёт
// тот самый аудит-сигнал «кто и когда выгружал непомаскированные данные»,
// который спека оставляет админу). Админу (CanManage) чужая заявка обязана
// быть видна целиком, включая кнопки download/delete.
//
// До ревью веб-части E1 (п.2) чужая заявка оператору БЫЛА видна строкой
// (kind/format/статус/email автора/PII), только кнопки download/delete были
// спрятаны — это и было дефектом: сама видимость строки утекала.
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
	// register() в newExportsStack заводит админа с этим email — строка чужой
	// заявки не должна рендериться вовсе, а не просто прятать кнопки.
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

// TestExportsPageBatchesAuthorEmailsAcrossDistinctAuthors — email автора
// собирается батчем через Auth.UserEmails (ревью веб-части E1, п.5: раньше
// exportViewRow звала h.Auth.UserEmail на КАЖДУЮ строку — N+1 к PG на
// рендер). Правильность батча проверяем напрямую: две заявки РАЗНЫХ
// авторов в одном проекте, админ (CanManage) видит обе через ByProject —
// оба email обязаны дойти до страницы, каждый ровно на свою строку, не
// перепутанные и не потерянные при разрешении по карте authorEmails.
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

// rowContaining вырезает содержимое ближайшего <tr>…</tr>, несущего marker
// (ссылку на конкретную заявку), — чтобы сравнивать email не по всей
// странице целиком (там оба присутствуют), а по конкретной строке таблицы.
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
