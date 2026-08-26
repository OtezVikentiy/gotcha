package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeIssueSource стримит n синтетических записей issues, игнорируя фильтр
// заявки: воркеру для этих тестов важна только сборка файла, а не то, что
// именно отфильтровано.
type fakeIssueSource struct {
	n       int
	failAt  int // индекс записи, на которой вернуть failErr вместо записи
	failErr error
}

func fakeIssues(n int) IssueSource { return &fakeIssueSource{n: n} }

// failingSource — источник, падающий на первой же записи (failAt=0 — до
// единой записи).
func failingSource(err error) IssueSource {
	return &fakeIssueSource{n: 1, failAt: 0, failErr: err}
}

// partialFailingSource пишет k записей и затем возвращает err — проверка,
// что .part не остаётся и от ошибки посреди потока, а не только от мгновенной.
func partialFailingSource(k int, err error) IssueSource {
	return &fakeIssueSource{n: k + 1, failAt: k, failErr: err}
}

func (s *fakeIssueSource) Stream(ctx context.Context, projectID int64, p Params, fn func(Record) error) error {
	for i := 0; i < s.n; i++ {
		if s.failErr != nil && i == s.failAt {
			return s.failErr
		}
		if err := fn(Record{
			"id": int64(i + 1), "title": fmt.Sprintf("issue %d", i+1),
			"culprit": "", "level": "error", "status": "unresolved",
			"times_seen": int64(1), "environments": "", "assignee_email": "", "url": "",
		}); err != nil {
			return err
		}
	}
	return nil
}

// fakeEventSource — аналог fakeIssueSource для kind=events, покрывает ветку
// columnsFor/stream, которую issues-сценарии не задевают.
type fakeEventSource struct{ n int }

func fakeEvents(n int) EventSource { return &fakeEventSource{n: n} }

func (s *fakeEventSource) Stream(ctx context.Context, projectID, scopeIssueID int64, p Params, fn func(Record) error) error {
	for i := 0; i < s.n; i++ {
		if err := fn(Record{
			"timestamp": time.Now().UTC(), "event_id": fmt.Sprintf("ev%d", i+1), "issue_id": int64(1),
			"level": "error", "message": "boom", "exception_type": "", "exception_value": "",
			"environment": "", "release": "", "server_name": "", "sdk": "", "trace_id": "",
			"user_id": "", "user_ip": "", "user_email": "", "tags": "",
		}); err != nil {
			return err
		}
	}
	return nil
}

// deleteOnStreamSource удаляет строку заявки из БД сразу после первой
// отданной записи — имитация «заявку снесли, пока воркер писал файл».
// Заявка к этому моменту уже в статусе running (Claim отработал раньше), так
// что удаление идёт напрямую, в обход Store.Delete (тот удаляет только
// терминальные заявки).
type deleteOnStreamSource struct {
	pool *pgxpool.Pool
	id   int64
}

func (s *deleteOnStreamSource) Stream(ctx context.Context, projectID int64, p Params, fn func(Record) error) error {
	if err := fn(Record{"id": int64(1), "title": "x", "culprit": "", "level": "error",
		"status": "unresolved", "times_seen": int64(1), "environments": "", "assignee_email": "", "url": ""}); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM export_jobs WHERE id = $1", s.id)
	return err
}

func TestWorkerWritesFileAndMarksDone(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone || j.RowsWritten != 3 || j.Bytes == 0 || j.Truncated {
		t.Fatalf("итог заявки: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.csv", id))); err != nil {
		t.Fatalf("файл выгрузки не создан: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.part", id))); !os.IsNotExist(err) {
		t.Error("остался временный .part-файл")
	}
}

func TestWorkerWritesEventsFile(t *testing.T) {
	// Ветка Kind=events (columnsFor/stream) не покрыта issues-сценариями
	// выше — отдельный проход с фиктивным EventSource.
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindEvents, FormatNDJSON)

	w := &Worker{Store: st, Pool: pool, Events: fakeEvents(2), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone || j.RowsWritten != 2 {
		t.Fatalf("итог заявки: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.ndjson", id))); err != nil {
		t.Fatalf("файл выгрузки не создан: %v", err)
	}
}

func TestWorkerTruncatesAtRowCap(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(50), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 2, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.RowsWritten != 2 || !j.Truncated || j.Status != StatusDone {
		t.Fatalf("обрезка не отмечена: строк=%d truncated=%v статус=%q", j.RowsWritten, j.Truncated, j.Status)
	}
}

func TestWorkerFailsPermanentlyOverDiskBudget(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	writeFiller(t, dir, 5<<20)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(100), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 20}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed {
		t.Fatalf("статус %q, ожидали failed без повторов", j.Status)
	}
	if j.Attempts != 1 {
		t.Errorf("сделано %d попыток — бессмысленный повтор постоянного отказа", j.Attempts)
	}
	if !strings.Contains(j.LastError, "мест") {
		t.Errorf("причина невнятна: %q", j.LastError)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.part", id))); !os.IsNotExist(err) {
		t.Error("бюджет проверяется ДО записи — .part не должен появляться вовсе")
	}
}

func TestWorkerLeavesNoPartFileOnFailure(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: failingSource(errors.New("ClickHouse недоступен")), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("после неудачи остался %s — мусор копится на каждой попытке", e.Name())
		}
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Отказ временный (ClickHouse недоступен — не наша вина): при
	// attempts=1 < maxAttempts заявка обязана вернуться в очередь, а не
	// осесть в failed после единственной попытки.
	if j.Status != StatusQueued || j.Attempts != 1 {
		t.Fatalf("временный отказ обработан как окончательный: %+v", j)
	}
}

func TestWorkerLeavesNoPartFileOnPartialWriteFailure(t *testing.T) {
	// Источник падает НЕ на первой записи — .part к моменту ошибки уже
	// непустой; проверка, что удаление .part не завязано на «файл пуст».
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: partialFailingSource(2, errors.New("сеть моргнула")), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("после обрыва посреди потока в каталоге остались файлы: %v", entries)
	}
}

// staleningIssueSource симулирует потерю лизы во время записи: пока воркер
// ещё пишет файл (Stream не вернулся), кто-то другой успевает переклеймить
// эту же заявку — attempts растёт в обход текущего вызова. К моменту Done
// связка status='running' AND attempts=$attempt из фенсинга Store.Fail/Done
// уже не совпадает с тем, что держит текущий вызов, и Done обязан получить
// ErrStaleClaim, а не дописать поверх чужой попытки.
type staleningIssueSource struct {
	pool *pgxpool.Pool
	id   int64
}

func (s *staleningIssueSource) Stream(ctx context.Context, projectID int64, p Params, fn func(Record) error) error {
	if err := fn(Record{"id": int64(1), "title": "x", "culprit": "", "level": "error",
		"status": "unresolved", "times_seen": int64(1), "environments": "", "assignee_email": "", "url": ""}); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "UPDATE export_jobs SET attempts = attempts + 1 WHERE id = $1", s.id)
	return err
}

func TestWorkerRemovesFileWhenLeaseLostBeforeDone(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: &staleningIssueSource{pool: pool, id: id}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Done должен был получить ErrStaleClaim (attempts из БД уже не совпадает
	// с тем, что держал вызов) и не отметить заявку как done: файл,
	// оставшийся на диске без подтверждённого владения, был бы скачиваемым
	// мусором зомби-попытки.
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status == StatusDone {
		t.Fatalf("зомби-попытка дописала заявку поверх чужого attempts: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.csv", id))); !os.IsNotExist(err) {
		t.Error("файл зомби-попытки остался на диске после потери лизы")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("после потери лизы остался %s", e.Name())
		}
	}
}

func TestWorkerDropsFileWhenJobDeletedMidFlight(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: &deleteOnStreamSource{pool: pool, id: id}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("файл осиротевшей заявки остался: %v", entries)
	}
}

func TestWorkerRequeuesOnMissingDir(t *testing.T) {
	// Каталог назначения отсутствует (в проде — фича вовсе не стартует, но
	// воркер сам по себе не должен терять ошибку записи: она обязана
	// доехать до Fail, а не пропасть молча).
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	root := t.TempDir()
	missing := filepath.Join(root, "does-not-exist")
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: missing, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued || j.Attempts != 1 || j.LastError == "" {
		t.Fatalf("отсутствие каталога не привело к внятному временному отказу: %+v", j)
	}
}

func TestWorkerRequeuesOnWritePermissionDenied(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // иначе t.TempDir() не сможет убрать за собой
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued || j.Attempts != 1 || j.LastError == "" {
		t.Fatalf("запрет на запись не довёл ошибку до заявки: %+v", j)
	}
}

func TestWorkerSkipsWhenAnotherInstanceHoldsLock(t *testing.T) {
	// Второй экземпляр воркера не должен начинать писать файл параллельно —
	// проверка бьёт по самому механизму exclusivity (advisory lock), держа
	// его на отдельном соединении, как это делала бы соседняя реплика.
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(advisoryLockKey)).Scan(&locked); err != nil || !locked {
		t.Fatalf("подготовка занятого лока: locked=%v err=%v", locked, err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(advisoryLockKey))

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued || j.Attempts != 0 {
		t.Fatalf("заявку тронули, пока лок держала другая реплика: %+v", j)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("файл создан, пока лок держала другая реплика: %v", entries)
	}
}

// TestJobTimeoutBelowLeaseTTL фиксирует инвариант, который иначе живёт только
// в комментарии рядом с константами: jobTimeout обязан быть строго меньше
// leaseTTL, иначе второй инстанс переклеймит заявку, которую первый ещё
// пишет. init() пакета уже паникует при нарушении — тест делает то же самое
// явным ассертом, который виден в отчёте прогона, а не только при падении.
func TestWorkerFailsPermanentlyWhenSourceNotConfigured(t *testing.T) {
	// Воркер, собранный без нужного источника (ошибка связки в cmd/, а не
	// временный сбой) — заявка не должна биться о ретраи: причина не
	// изменится сама собой.
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindEvents, FormatNDJSON)

	w := &Worker{Store: st, Pool: pool, Cfg: Config{ // Events не задан
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.Attempts != 1 {
		t.Fatalf("отсутствие источника не привело к постоянному отказу: %+v", j)
	}
}

func TestWorkerRunStopsOnContextCancel(t *testing.T) {
	// tickInterval — 5s, дожидаться настоящего тика в юнит-тесте незачем:
	// достаточно убедиться, что Run не виснет после отмены ctx до первого
	// срабатывания тикера.
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(0), Cfg: Config{
		Dir: t.TempDir(), TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		w.Run(runCtx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не остановился после отмены ctx")
	}
}

func TestJobTimeoutBelowLeaseTTL(t *testing.T) {
	if jobTimeout >= leaseTTL {
		t.Fatalf("jobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", jobTimeout, leaseTTL)
	}
}

func writeFiller(t *testing.T, dir string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "filler.bin"))
	if err != nil {
		t.Fatalf("writeFiller: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("writeFiller truncate: %v", err)
	}
}
