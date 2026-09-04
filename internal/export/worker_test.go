package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeIssueSource стримит n синтетических записей issues, игнорируя фильтр
// заявки: воркеру для этих тестов важна только сборка файла, а не то, что
// именно отфильтровано.
type fakeIssueSource struct {
	n       int
	failAt  int // индекс записи, на которой вернуть failErr вместо записи
	failErr error
	// seenPII — includePII, с которым Stream был вызван, по порядку вызовов
	// (мутационная проверка M1b, worker.go:stream: includePII обязан быть
	// job.IncludePII заявки, а не константой).
	seenPII []bool
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

func (s *fakeIssueSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	s.seenPII = append(s.seenPII, includePII)
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

func (s *fakeEventSource) Stream(ctx context.Context, projectID, scopeIssueID int64, includePII bool, p Params, fn func(Record) error) error {
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

func (s *deleteOnStreamSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
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

// TestWorkerFileModeExcludesOtherAccess — файл выгрузки — единственное
// место продукта, где ПДн ложатся на диск (P3-SEC-1 аудита): режим обязан
// быть 0600, а не 0644 (на bare-metal деплое 0644 читается любым
// пользователем хоста).
func TestWorkerFileModeExcludesOtherAccess(t *testing.T) {
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
	info, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.csv", id)))
	if err != nil {
		t.Fatalf("файл выгрузки не создан: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("режим файла выгрузки = %o, want 0600 (файл несёт ПДн)", mode)
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

// TestWorkerPassesJobIncludePIIToEventSource — мутационная проверка врезки
// worker.go:stream(): includePII, дошедший до EventSource.Stream, обязан
// быть галочкой ИМЕННО этой заявки (job.IncludePII), а не константой,
// зашитой в момент постройки w.Events. Источник — НАСТОЯЩИЙ eventSource
// (не фиктивный), заявки собираются одним и тем же Worker.Events одна за
// другой, а проверка идёт по РЕАЛЬНОМУ содержимому файлов на диске: если
// бы стрим вызывался с захардкоженным true/false (или с полем самого
// источника, как было до фикса), одна из двух заявок ниже либо унесла бы
// PII под маской «отфильтровано», либо отдала бы маску там, где заявка
// просила «выгрузить как есть». Проверка по именам ключей здесь недостаточна
// — только по фактическим секретным значениям в байтах файла.
func TestWorkerPassesJobIncludePIIToEventSource(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	st := NewStore(pool)
	svc := issue.NewService(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	now := time.Now().UTC()

	const leakEmail = "leak-worker-pii@example.com"
	const leakIP = "203.0.113.42"

	res, err := svc.Upsert(ctx, projectID, "fp-worker-pii", "boom", "app.worker", issue.LevelError, "prod", now)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	b := event.NewBatcher(ch)
	go b.Run()
	b.Add(event.Event{
		ID: uuid.NewString(), ProjectID: projectID, IssueID: res.IssueID, Timestamp: now,
		Message: "boom", Level: issue.LevelError, Environment: "prod",
		UserIP: leakIP, UserEmail: leakEmail,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	since, until := now.Add(-time.Hour), now.Add(time.Hour)
	maskedID, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID, Kind: KindEvents, Format: FormatNDJSON,
		Params: Params{Since: since, Until: until}, IncludePII: false,
	})
	if err != nil {
		t.Fatalf("Enqueue (masked): %v", err)
	}
	rawID, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID, Kind: KindEvents, Format: FormatNDJSON,
		Params: Params{Since: since, Until: until}, IncludePII: true,
	})
	if err != nil {
		t.Fatalf("Enqueue (raw): %v", err)
	}

	w := &Worker{Store: st, Pool: pool, Events: NewEventSource(event.NewQuery(ch), svc), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}

	// Claim берёт по ORDER BY created_at — сначала masked, потом raw, один
	// и тот же w.Events на оба тика.
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	if maskedJob, err := st.Get(ctx, maskedID); err != nil || maskedJob.Status != StatusDone {
		t.Fatalf("masked job: status=%+v err=%v", maskedJob, err)
	}
	if rawJob, err := st.Get(ctx, rawID); err != nil || rawJob.Status != StatusDone {
		t.Fatalf("raw job: status=%+v err=%v", rawJob, err)
	}

	maskedOut, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.ndjson", maskedID)))
	if err != nil {
		t.Fatalf("чтение файла masked-заявки: %v", err)
	}
	if strings.Contains(string(maskedOut), leakEmail) || strings.Contains(string(maskedOut), leakIP) {
		t.Errorf("PII утекло в выгрузку заявки с IncludePII=false: %s", maskedOut)
	}

	rawOut, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.ndjson", rawID)))
	if err != nil {
		t.Fatalf("чтение файла raw-заявки: %v", err)
	}
	if !strings.Contains(string(rawOut), leakEmail) || !strings.Contains(string(rawOut), leakIP) {
		t.Errorf("реальные значения отсутствуют в выгрузке заявки с IncludePII=true (галка проигнорирована): %s", rawOut)
	}
}

// TestWorkerPassesJobIncludePIIToIssueSource — то же самое (M1b, аудит
// перед 1.0), что TestWorkerPassesJobIncludePIIToEventSource, но для
// worker.go:stream ветки KindIssues: includePII, дошедший до
// IssueSource.Stream, обязан быть галочкой ИМЕННО этой заявки
// (job.IncludePII), а не константой, зашитой в вызове (например, true
// независимо от заявки — тогда маска assignee_email из source_issues.go
// никогда не применялась бы). fakeIssueSource фиксирует includePII каждого
// вызова Stream, Claim берёт заявки по ORDER BY created_at — сначала
// masked, потом raw.
func TestWorkerPassesJobIncludePIIToIssueSource(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	now := time.Now().UTC()

	maskedID, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID, Kind: KindIssues, Format: FormatCSV,
		Params: Params{Since: now.Add(-time.Hour), Until: now}, IncludePII: false,
	})
	if err != nil {
		t.Fatalf("Enqueue (masked): %v", err)
	}
	rawID, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID, Kind: KindIssues, Format: FormatCSV,
		Params: Params{Since: now.Add(-time.Hour), Until: now}, IncludePII: true,
	})
	if err != nil {
		t.Fatalf("Enqueue (raw): %v", err)
	}

	src := &fakeIssueSource{n: 1}
	w := &Worker{Store: st, Pool: pool, Issues: src, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	if maskedJob, err := st.Get(ctx, maskedID); err != nil || maskedJob.Status != StatusDone {
		t.Fatalf("masked job: status=%+v err=%v", maskedJob, err)
	}
	if rawJob, err := st.Get(ctx, rawID); err != nil || rawJob.Status != StatusDone {
		t.Fatalf("raw job: status=%+v err=%v", rawJob, err)
	}

	if want := []bool{false, true}; len(src.seenPII) != len(want) || src.seenPII[0] != want[0] || src.seenPII[1] != want[1] {
		t.Errorf("IssueSource.Stream вызван с includePII=%v, want %v (masked заявка первой, raw второй)", src.seenPII, want)
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

// TestWorkerRetriesOnDiskBudgetExceeded — P3-SEC-3 аудита: «места нет прямо
// сейчас» самоустраняется первым же проходом джанитора (файлы истекают,
// освобождают бюджет), в отличие от ErrTooManyIssues (постоянная причина,
// требующая действия автора) — disk_full обязан вернуться в очередь
// (fail(), 3 попытки), а не отказать НАВСЕГДА первой же попыткой
// (failPermanent валил бы выгрузки чужих организаций до ручного
// вмешательства, пока один тенант держит диск полным).
func TestWorkerRetriesOnDiskBudgetExceeded(t *testing.T) {
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
	if j.Status != StatusQueued {
		t.Fatalf("статус %q, ожидали queued — disk_full обязан быть ВРЕМЕННЫМ отказом", j.Status)
	}
	if j.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (первая временная неудача)", j.Attempts)
	}
	if !strings.Contains(j.LastError, "мест") {
		t.Errorf("причина невнятна: %q", j.LastError)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.part", id))); !os.IsNotExist(err) {
		t.Error("бюджет проверяется ДО записи — .part не должен появляться вовсе")
	}
}

// TestWorkerNotifiesOnDiskFullAfterFinalAttempt — disk_full теперь временный
// отказ (fail(), см. TestWorkerRetriesOnDiskBudgetExceeded): Notify обязан
// сработать только на ПОСЛЕДНЕЙ попытке (maxAttempts), не на первой, и
// донести ИМЕННО reasonDiskFull автору письма (даёт понятное действие —
// подождать), а не общий reasonInternal.
func TestWorkerNotifiesOnDiskFullAfterFinalAttempt(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	writeFiller(t, dir, 5<<20)

	notified := make(chan Job, maxAttempts)
	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(100), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 20,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}

	for i := 0; i < maxAttempts; i++ {
		if err := w.Tick(ctx); err != nil {
			t.Fatalf("Tick #%d: %v", i+1, err)
		}
		if i < maxAttempts-1 && len(notified) != 0 {
			t.Fatalf("Notify вызван до исчерпания попыток (попытка %d)", i+1)
		}
	}

	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.Attempts != maxAttempts {
		t.Fatalf("заявка не дошла до окончательного отказа: %+v", j)
	}

	select {
	case notifiedJob := <-notified:
		if notifiedJob.ID != id || notifiedJob.Status != StatusFailed || notifiedJob.LastError == "" {
			t.Fatalf("Notify получил неожиданный снимок заявки: %+v", notifiedJob)
		}
		// reasonDiskFull, не reasonInternal — мутация switch на reasonInternal
		// осталась бы незамеченной без этой проверки.
		if notifiedJob.FailureReasonKey != reasonDiskFull {
			t.Fatalf("FailureReasonKey = %q, want %q", notifiedJob.FailureReasonKey, reasonDiskFull)
		}
	default:
		t.Fatal("Notify не вызван после исчерпания попыток")
	}
	if len(notified) != 0 {
		t.Fatalf("Notify вызван более одного раза: ещё %d в очереди", len(notified))
	}
}

// TestWorkerReservesBudgetHeadroomForCurrentJob — P2-OPS-4 аудита: раньше
// проверка была used >= DiskBudget, и заявка при used == DiskBudget-1
// проходила, а затем дописывала до MaxBytes СВЕРХ бюджета — единственный
// потолок размера файла её не сдерживал, потому что заявку уже пропустили.
// used настроен РОВНО на 1 байт меньше бюджета — граница, которую старая
// проверка молча пропускала.
func TestWorkerReservesBudgetHeadroomForCurrentJob(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	const diskBudget = 1 << 20
	writeFiller(t, dir, diskBudget-1) // used = budget-1: бюджет формально не исчерпан

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(100), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: diskBudget}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("статус %q, ожидали queued — заявка не резервирует MaxBytes сверх бюджета и обязана отказать", j.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.part", id))); !os.IsNotExist(err) {
		t.Error("бюджет проверяется ДО записи — .part не должен появляться вовсе")
	}
}

// TestWorkerRejectsOnLowRealDiskSpace — P2-OPS-4 аудита: в поставляемом
// docker-compose pgdata/chdata/exportdata делят одну файловую систему хоста,
// поэтому заявка, уместившаяся в Config.DiskBudget, всё равно может не
// уместиться на РЕАЛЬНОМ диске — Worker.FreeBytes (инъекция для теста)
// сообщает мало свободного места, хотя каталог выгрузок пуст.
func TestWorkerRejectsOnLowRealDiskSpace(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(100), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30},
		FreeBytes: func(string) (int64, bool, error) { return 100, true, nil }, // меньше MaxBytes
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("статус %q, ожидали queued — реального места на ФС не хватает, хотя каталог-бюджет свободен", j.Status)
	}
	if !strings.Contains(j.LastError, "файловой системе") {
		t.Errorf("причина не различает нехватку бюджета и нехватку места на ФС: %q", j.LastError)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.part", id))); !os.IsNotExist(err) {
		t.Error(".part не должен появляться вовсе")
	}
}

// TestWorkerIgnoresRealDiskSpaceWhenUnsupported — Worker.FreeBytes с ok=false
// (платформа без Statfs, см. diskfree_other.go) обязан оставить решение
// ЦЕЛИКОМ за бюджетом — заявка, вписывающаяся в DiskBudget, не должна
// отказывать из-за неподдержанной проверки реального диска.
func TestWorkerIgnoresRealDiskSpaceWhenUnsupported(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30},
		FreeBytes: func(string) (int64, bool, error) { return 0, false, nil }, // не поддержано
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone {
		t.Fatalf("статус %q, ожидали done — ok=false обязан пропускать проверку реального диска", j.Status)
	}
}

// TestWorkerNotifiesTooManyGroupsReason — ErrTooManyIssues от источника
// (фильтр резолвится в слишком много групп) обязан дать письму СВОЙ ключ
// причины (reasonTooManyGroups, «сузьте условия»), а не общий
// reasonInternal: это единственная постоянная причина, которую автор может
// устранить сам (§8 спеки).
func TestWorkerNotifiesTooManyGroupsReason(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	notified := make(chan Job, 1)
	w := &Worker{Store: st, Pool: pool, Issues: failingSource(ErrTooManyIssues), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.Attempts != 1 {
		t.Fatalf("ErrTooManyIssues обязан быть постоянным отказом с первой попытки: %+v", j)
	}

	select {
	case notifiedJob := <-notified:
		if notifiedJob.FailureReasonKey != reasonTooManyGroups {
			t.Fatalf("FailureReasonKey = %q, want %q", notifiedJob.FailureReasonKey, reasonTooManyGroups)
		}
	default:
		t.Fatal("Notify не вызван после постоянного отказа ErrTooManyIssues")
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

// TestWorkerDoesNotNotifyOnRetryableFailure — временный отказ первой
// попытки не сообщает автору: заявка ещё может досчитаться со следующего
// тика, письмо о ней было бы преждевременным.
func TestWorkerDoesNotNotifyOnRetryableFailure(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	notified := make(chan Job, 1)
	w := &Worker{Store: st, Pool: pool, Issues: failingSource(errors.New("ClickHouse недоступен")), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	select {
	case j := <-notified:
		t.Fatalf("Notify вызван на первой (retryable) попытке: %+v", j)
	default:
	}
}

// TestWorkerNotifiesOnFinalRetryableFailure — та же временная причина
// отказа, но заявка исчерпала все maxAttempts попыток: Store.Fail сама
// переводит её в failed, и это тот самый момент, когда автору наконец стоит
// написать (ровно один раз, не на каждой из промежуточных попыток).
func TestWorkerNotifiesOnFinalRetryableFailure(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	notified := make(chan Job, maxAttempts)
	w := &Worker{Store: st, Pool: pool, Issues: failingSource(errors.New("ClickHouse недоступен")), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}

	for i := 0; i < maxAttempts; i++ {
		if err := w.Tick(ctx); err != nil {
			t.Fatalf("Tick #%d: %v", i+1, err)
		}
	}

	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.Attempts != maxAttempts {
		t.Fatalf("заявка не дошла до окончательного отказа: %+v", j)
	}

	select {
	case notifiedJob := <-notified:
		if notifiedJob.ID != id || notifiedJob.Status != StatusFailed {
			t.Fatalf("Notify получил неожиданный снимок заявки: %+v", notifiedJob)
		}
		// Транзитная инфраструктурная причина ("ClickHouse недоступен") не
		// даёт автору вменяемого действия — общий reasonInternal, не
		// reasonDiskFull/reasonTooManyGroups.
		if notifiedJob.FailureReasonKey != reasonInternal {
			t.Fatalf("FailureReasonKey = %q, want %q", notifiedJob.FailureReasonKey, reasonInternal)
		}
	default:
		t.Fatal("Notify не вызван после исчерпания попыток")
	}
	if len(notified) != 0 {
		t.Fatalf("Notify вызван более одного раза: ещё %d в очереди", len(notified))
	}
}

// TestWorkerNotifiesOnSweepStale — заявка, зависшая вместе с погибшим
// инстансом на последней попытке (running, attempts=maxAttempts, лиза
// протухла), добивается не через fail()/failPermanent() воркера, а через
// Store.SweepStale в начале Tick — это единственный терминальный исход
// фичи, о котором Worker.process не узнаёт вовсе. Раньше Tick вызывал
// SweepStale только ради счётчика и никого не уведомлял: автор заявки не
// получал письма и не мог узнать об отказе иначе как перезагрузкой страницы
// выгрузок (см. §9 спеки — письмо обязательно на КАЖДОМ терминальном
// исходе).
func TestWorkerNotifiesOnSweepStale(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=$2, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id, maxAttempts); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	notified := make(chan Job, 1)
	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	select {
	case j := <-notified:
		if j.ID != id || j.Status != StatusFailed {
			t.Fatalf("Notify получил неожиданный снимок заявки: %+v", j)
		}
		if j.FailureReasonKey != reasonInternal {
			t.Fatalf("FailureReasonKey = %q, want %q", j.FailureReasonKey, reasonInternal)
		}
	default:
		t.Fatal("Notify не вызван после SweepStale")
	}
	if len(notified) != 0 {
		t.Fatalf("Notify вызван более одного раза: ещё %d в очереди", len(notified))
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

func (s *staleningIssueSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
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

// TestWorkerFailsPermanentlyWhenSourceNotConfigured — воркер, собранный без
// нужного источника (ошибка связки в cmd/, а не временный сбой) — заявка не
// должна биться о ретраи: причина не изменится сама собой.
func TestWorkerFailsPermanentlyWhenSourceNotConfigured(t *testing.T) {
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

// TestWorkerFailsPermanentlyWhenIssueSourceNotConfigured — симметричный
// случай для groups: до сих пор был покрыт только KindEvents/Events==nil.
func TestWorkerFailsPermanentlyWhenIssueSourceNotConfigured(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Cfg: Config{ // Issues не задан
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.Attempts != 1 {
		t.Fatalf("отсутствие источника групп не привело к постоянному отказу: %+v", j)
	}
}

// TestTickReturnsNilWhenQueueEmpty — очередь пуста, Claim отдаёт ok=false:
// Tick обязан молча выйти, не тронув ничего и не вернув ошибку.
func TestTickReturnsNilWhenQueueEmpty(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	w := &Worker{Store: st, Pool: pool, Cfg: Config{
		Dir: t.TempDir(), TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick по пустой очереди: %v", err)
	}
}

// TestWorkerRejectsMaxRowsAtOrAboveSafetyLimit — P1: GOTCHA_EXPORT_MAX_ROWS
// на уровне защитного предела потока событий (eventStreamSafetyLimit)
// обесценивает Truncated — источник событий физически не отдаст больше
// eventStreamSafetyLimit строк, поток кончится «естественно» раньше, чем
// счётчик заявки дойдёт до своего потолка, и пользователь получит молча
// усечённую выгрузку. Config.Validate (вызывается из Tick) обязан упасть
// внятной ошибкой конфигурации ДО клейма заявки, а не тихо продолжить.
func TestWorkerRejectsMaxRowsAtOrAboveSafetyLimit(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindEvents, FormatNDJSON)

	w := &Worker{Store: st, Pool: pool, Events: fakeEvents(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: eventStreamSafetyLimit, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	err := w.Tick(ctx)
	if err == nil {
		t.Fatal("MaxRows на уровне защитного предела — ожидали ошибку конфигурации")
	}
	if !strings.Contains(err.Error(), "Truncated") {
		t.Errorf("причина невнятна: %v", err)
	}

	// Заявка не тронута вовсе: конфигурация проверяется раньше клейма.
	j, getErr := st.Get(ctx, id)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if j.Status != StatusQueued || j.Attempts != 0 {
		t.Fatalf("заявку тронули при неверной конфигурации: %+v", j)
	}
}

// TestWorkerAcceptsMaxRowsBelowSafetyLimit — симметричный положительный
// случай: значение строго ниже предела не мешает обычной сборке файла.
func TestWorkerAcceptsMaxRowsBelowSafetyLimit(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindEvents, FormatNDJSON)

	w := &Worker{Store: st, Pool: pool, Events: fakeEvents(3), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: eventStreamSafetyLimit - 1, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone {
		t.Fatalf("итог заявки: %+v", j)
	}
}

// TestWorkerTruncatesAtByteCapIndependentlyOfRowCap — потолок байт должен
// сработать сам по себе, а не только как побочный эффект потолка строк: до
// сих пор все тесты держали MaxBytes заведомо просторным.
func TestWorkerTruncatesAtByteCapIndependentlyOfRowCap(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(50), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 1000, MaxBytes: 200, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !j.Truncated || j.RowsWritten == 0 || j.RowsWritten >= 50 {
		t.Fatalf("потолок байт не сработал независимо от потолка строк: %+v", j)
	}
}

// badRecordSource отдаёт запись, которую JSON-писатель не может
// сериализовать (канал — не JSON-тип), — источник ошибки внутри самой
// записи строки, а не чтения источника.
type badRecordSource struct{}

func (badRecordSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	return fn(Record{"bad": make(chan int)})
}

func TestWorkerLeavesNoPartFileOnSerializationFailure(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatJSON)

	w := &Worker{Store: st, Pool: pool, Issues: badRecordSource{}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued || j.Attempts != 1 || j.LastError == "" {
		t.Fatalf("ошибка сериализации не довелась до заявки: %+v", j)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("после ошибки сериализации в каталоге остались файлы: %v", entries)
	}
}

// staleningFailingSource симулирует потерю лизы, совпавшую с транзитным
// отказом: пока источник ещё «читается», кто-то другой успевает
// переклеймить эту же заявку (attempts растёт в обход текущего вызова), а
// сам источник в довершение возвращает обычную (не постоянную) ошибку.
// worker.fail обязан получить ErrStaleClaim от Store.Fail и проглотить её
// молча — активная попытка чужого клейма не должна быть задета отказом
// зомби-вызова.
type staleningFailingSource struct {
	pool *pgxpool.Pool
	id   int64
}

func (s *staleningFailingSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	if _, err := s.pool.Exec(ctx, "UPDATE export_jobs SET attempts = attempts + 1 WHERE id = $1", s.id); err != nil {
		return err
	}
	return errors.New("сеть моргнула")
}

func TestWorkerFailSuppressesStaleClaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: &staleningFailingSource{pool: pool, id: id}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusRunning || j.Attempts != 2 {
		t.Fatalf("Fail задел чужую попытку при протухшей лизе: %+v", j)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("после потери лизы в fail-ветке остался %s", e.Name())
		}
	}
}

// slowAfterWriteSource пишет одну запись сразу (пока ctx ещё жив), затем
// ждёт истечения ctx.Done() (гарантированно совпадает с дедлайном тика,
// сколько бы Claim/SweepStale/открытие файла ни заняли на медленном
// раннере) плюс небольшой запас и только потом возвращается. Запись успевает
// пройти ДО истечения дедлайна, поэтому writeFile завершается успешно и
// process доходит до Store.Done — с уже просроченным ctx. Если бы Done
// вызывался с context.Background() вместо этого ctx, отказ бы не наступил и
// заявка стала бы done несмотря на истёкший тайм-аут тика.
type slowAfterWriteSource struct{}

func (s *slowAfterWriteSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	if err := fn(Record{"id": int64(1), "title": "x", "culprit": "", "level": "error",
		"status": "unresolved", "times_seen": int64(1), "environments": "", "assignee_email": "", "url": ""}); err != nil {
		return err
	}
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	return nil
}

func TestWorkerDoesNotFinalizeAfterJobTimeoutExpiresBeforeDone(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: &slowAfterWriteSource{}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
		JobTimeout: time.Second, // с запасом над Claim/SweepStale/открытием файла
	}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status == StatusDone {
		t.Fatal("заявка завершилась успехом ПОСЛЕ истечения JobTimeout — Done вызван с неограниченным ctx вместо контекста тика")
	}
}

// TestWorkerRemovesFileWhenDoneFailsNotStaleClaim воспроизводит ту же гонку,
// что и TestWorkerDoesNotFinalizeAfterJobTimeoutExpiresBeforeDone (jobCtx
// истёк между rename и Store.Done), но проверяет другую половину дефекта:
// Store.Done с уже истёкшим ctx возвращает ошибку контекста, а не
// ErrStaleClaim (ноль строк там не при чём — pgx не успевает даже уйти в
// сеть). Раньше уборка finalPath была только в ветке ErrStaleClaim — эта
// ошибка утекала мимо неё, и файл навсегда оставался на диске при заявке,
// которую позже добьёт SweepStale в failed (заявка недостижима для
// скачивания, но место на диске не освобождается вплоть до PurgeRows).
func TestWorkerRemovesFileWhenDoneFailsNotStaleClaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	w := &Worker{Store: st, Pool: pool, Issues: &slowAfterWriteSource{}, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
		JobTimeout: time.Second, // с запасом над Claim/SweepStale/открытием файла
	}}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status == StatusDone {
		t.Fatal("заявка неожиданно завершилась успехом")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("после отказа Done (не ErrStaleClaim) в каталоге выгрузок остался %s", e.Name())
	}
}

// shutdownIssueSource симулирует остановку процесса (SIGTERM/деплой,
// P2-OPS-5) в разгар сборки: после n записей источник сам отменяет runCtx —
// тот же ctx, что Worker.Tick получил снаружи, — и возвращает его ошибку
// отмены, как это сделал бы реальный источник (ClickHouse/PG), у которого
// запрос оборвался вместе с ctx. Worker.process обязан отличить эту отмену
// РОДИТЕЛЬСКОГО ctx от настоящего сбоя сборки и вернуть заявку в очередь
// через release(), не потратив попытку через fail().
type shutdownIssueSource struct {
	n      int
	cancel context.CancelFunc
}

func (s *shutdownIssueSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	for i := 0; i < s.n; i++ {
		if err := fn(Record{"id": int64(i + 1), "title": "x", "culprit": "", "level": "error",
			"status": "unresolved", "times_seen": int64(1), "environments": "", "assignee_email": "", "url": ""}); err != nil {
			return err
		}
	}
	s.cancel()
	<-ctx.Done()
	return ctx.Err()
}

// TestWorkerReleasesJobOnShutdownDuringBuild — основной сценарий P2-OPS-5:
// SIGTERM/деплой ловят заявку посреди сборки. Мутация — убрать ветку
// "runCtx.Err() != nil" в process() (или проверять jobCtx вместо runCtx) —
// обязана уронить это тест: заявка либо потратит попытку через fail()
// (attempts станет 2), либо (без детача в release()) навсегда останется
// running, потому что Store.Release получит уже отменённый ctx.
func TestWorkerReleasesJobOnShutdownDuringBuild(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &shutdownIssueSource{n: 2, cancel: cancel}
	w := &Worker{Store: st, Pool: pool, Issues: src, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(runCtx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	j, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("заявка при остановке процесса не вернулась в очередь: %+v", j)
	}
	if j.Attempts != 1 {
		t.Fatalf("остановка процесса потратила попытку заявки: attempts=%d, ожидали 1", j.Attempts)
	}
	if j.LastError != "" || j.FailureReasonKey != "" {
		t.Fatalf("release() заполнил поля отказа: last_error=%q reason_key=%q", j.LastError, j.FailureReasonKey)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("после release() при остановке процесса в каталоге остался %s", e.Name())
	}
}

// TestWorkerDoesNotWarnOnAdvisoryUnlockDuringShutdown — снятие advisory lock
// в Tick() пишется через detachTimeout(ctx), а не ctx напрямую (P2-OPS-5):
// ctx — Run-level контекст, отменённый к моменту, когда отработавший до
// конца тик доходит до отложенного pg_advisory_unlock. Без детача это не
// баг (лок сессионный, соединение его переустановит само), но WARN на
// КАЖДОМ деплое приучает оператора игнорировать предупреждения в логе.
// Мутация — вернуть в defer'е conn.Exec(ctx, ...) вместо detachTimeout(ctx)
// — обязана уронить этот тест: снятие лока получит уже отменённый ctx,
// провалится и залогирует "снятие advisory lock".
func TestWorkerDoesNotWarnOnAdvisoryUnlockDuringShutdown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &shutdownIssueSource{n: 2, cancel: cancel}
	w := &Worker{Store: st, Pool: pool, Issues: src, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(runCtx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if strings.Contains(logBuf.String(), "снятие advisory lock") {
		t.Fatalf("снятие advisory lock залогировало WARN при штатной остановке процесса: %s", logBuf.String())
	}
}

// permanentFailAfterShutdownSource отменяет переданный runCtx (имитируя
// SIGTERM/деплой, совпавший по времени с постоянным отказом сборки — редкая,
// но возможная гонка), а затем всё равно возвращает permErr, никак не
// связанный с отменой ctx. Настоящий постоянный отказ обязан остаться
// постоянным отказом (failPermanent), а не замаскироваться под безобидный
// release() только потому, что процесс в этот же момент останавливают —
// иначе конфигурационная проблема молча повторялась бы на каждом старте, а
// автор заявки никогда не получил бы письма с внятной причиной.
type permanentFailAfterShutdownSource struct {
	cancel  context.CancelFunc
	permErr error
}

func (s *permanentFailAfterShutdownSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	s.cancel()
	return s.permErr
}

// TestWorkerStillFailsPermanentlyDuringShutdown — проверяет порядок веток в
// process(): permanent-случаи обязаны проверяться РАНЬШЕ runCtx.Err().
// Мутация — переставить "case runCtx.Err() != nil" перед
// "case errors.Is(err, ErrTooManyIssues)"/ErrPermanent — обязана уронить
// этот тест: заявка станет queued вместо failed с reasonTooManyGroups.
func TestWorkerStillFailsPermanentlyDuringShutdown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &permanentFailAfterShutdownSource{cancel: cancel, permErr: ErrTooManyIssues}
	w := &Worker{Store: st, Pool: pool, Issues: src, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(runCtx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	j, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusFailed || j.FailureReasonKey != reasonTooManyGroups {
		t.Fatalf("остановка процесса замаскировала постоянный отказ под release(): %+v", j)
	}
}

// shutdownAfterLastWriteSource пишет одну запись, затем сам отменяет
// переданный runCtx (имитируя SIGTERM/деплой, поймавший заявку РОВНО между
// последней записью и Store.Done) и возвращает успех. writeFile поэтому
// завершается успешно, но jobCtx на момент вызова Done уже мёртв через свою
// связь с runCtx — Done обязан всё равно записать успех через
// detachTimeout(), а не оставить заявку running до SweepStale (P2-OPS-5).
type shutdownAfterLastWriteSource struct {
	cancel context.CancelFunc
}

func (s *shutdownAfterLastWriteSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	if err := fn(Record{"id": int64(1), "title": "x", "culprit": "", "level": "error",
		"status": "unresolved", "times_seen": int64(1), "environments": "", "assignee_email": "", "url": ""}); err != nil {
		return err
	}
	s.cancel()
	return nil
}

// TestWorkerFinalizesDoneDespiteShutdownRacingCompletion — мутация: заменить
// detachTimeout(ctx) на ctx напрямую в Done-ветке process() — обязана
// уронить этот тест (заявка останется running вместо done), потому что
// jobCtx унаследовал отмену от runCtx и запись в PG с уже отменённым ctx
// проваливается мгновенно.
func TestWorkerFinalizesDoneDespiteShutdownRacingCompletion(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &shutdownAfterLastWriteSource{cancel: cancel}
	w := &Worker{Store: st, Pool: pool, Issues: src, Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30}}
	if err := w.Tick(runCtx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	j, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone {
		t.Fatalf("Done не пережил остановку процесса, догнавшую завершение сборки: %+v", j)
	}
}

// TestFailDetachesFromCanceledParentContext — прямой тест на detachTimeout():
// родитель отменяется ДО вызова fail(), запись итога всё равно обязана
// дойти до PG. Мутация — вызвать Store.Fail(ctx, ...) вместо
// Store.Fail(dctx, ...) в fail() — обязана уронить этот тест: PG-запрос с
// уже отменённым ctx проваливается мгновенно, и статус заявки останется
// running вместо queued.
func TestFailDetachesFromCanceledParentContext(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	claim, ok, err := st.Claim(context.Background())
	if err != nil || !ok {
		t.Fatalf("Claim: %v ok=%v", err, ok)
	}

	canceled, cancelParent := context.WithCancel(context.Background())
	cancelParent() // родитель отменён ДО вызова fail()

	w := &Worker{Store: st}
	w.fail(canceled, claim, errors.New("сбой сборки"), reasonInternal)

	got, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued || got.LastError != "сбой сборки" {
		t.Fatalf("fail() не записал итог с отменённым родительским ctx: %+v", got)
	}
}

// TestReleaseDetachesFromCanceledParentContext — то же самое для release():
// см. докблок TestFailDetachesFromCanceledParentContext.
func TestReleaseDetachesFromCanceledParentContext(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	claim, ok, err := st.Claim(context.Background())
	if err != nil || !ok {
		t.Fatalf("Claim: %v ok=%v", err, ok)
	}

	canceled, cancelParent := context.WithCancel(context.Background())
	cancelParent() // родитель отменён ДО вызова release()

	w := &Worker{Store: st}
	w.release(canceled, claim)

	got, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued || got.Attempts != claim.Attempts {
		t.Fatalf("release() не записал итог с отменённым родительским ctx: %+v", got)
	}
}

// TestWorkerRunProcessesJobOnTicker — достигает ветку <-ticker.C в Run
// (до сих пор все тесты били по Tick напрямую) и заодно w.Notify — тоже не
// вызывавшийся ни в одном тесте.
func TestWorkerRunProcessesJobOnTicker(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	notified := make(chan Job, 1)
	w := &Worker{Store: st, Pool: pool, Issues: fakeIssues(2), Cfg: Config{
		Dir: dir, TTL: time.Hour, MaxRows: 100, MaxBytes: 1 << 20, DiskBudget: 1 << 30,
		TickInterval: 10 * time.Millisecond,
	}, Notify: func(ctx context.Context, j Job) { notified <- j }}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(runCtx)

	select {
	case j := <-notified:
		if j.ID != id || j.Status != StatusDone || j.RowsWritten != 2 {
			t.Fatalf("Notify получил неожиданный снимок заявки: %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run не обработал заявку по тикеру за отведённое время")
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

// TestJobTimeoutBelowLeaseTTL фиксирует инвариант, который иначе живёт
// только в комментарии рядом с константами: defaultJobTimeout обязан быть
// строго меньше leaseTTL, иначе второй инстанс переклеймит заявку, которую
// первый ещё пишет. init() пакета уже паникует при нарушении — тест делает
// то же самое явным ассертом, который виден в отчёте прогона, а не только
// при падении. Инвариант для значения, заданного через Config (а не
// дефолтного), проверяет Config.Validate — см. TestWorkerDoesNotFinalize...
// и TestWorkerRejectsMaxRowsAtOrAboveSafetyLimit для JobTimeout из Tick.
func TestJobTimeoutBelowLeaseTTL(t *testing.T) {
	if defaultJobTimeout >= leaseTTL {
		t.Fatalf("defaultJobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", defaultJobTimeout, leaseTTL)
	}
}

// TestConfigValidateRejectsJobTimeoutAtOrAboveLeaseTTL — инъекция
// Config.JobTimeout не должна обходить инвариант, который для дефолтного
// значения держит init()-паника: значение из окружения (§10 спеки) так же
// легко развести с leaseTTL, как и константы кода.
func TestConfigValidateRejectsJobTimeoutAtOrAboveLeaseTTL(t *testing.T) {
	cfg := validExportConfig()
	cfg.JobTimeout = leaseTTL
	if err := cfg.Validate(); err == nil {
		t.Fatal("JobTimeout == leaseTTL — ожидали ошибку конфигурации")
	}
}

// validExportConfig — конфигурация, проходящая Validate() целиком, чтобы
// тесты на ОДНО конкретное поле не зависели от порядка проверок внутри
// Validate() и не проходили случайно из-за того, что более раннее поле уже
// невалидно.
func validExportConfig() Config {
	return Config{
		MaxRows:    1000,
		MaxBytes:   1 << 20,
		DiskBudget: 1 << 30,
		TTL:        time.Hour,
	}
}

// TestConfigValidateRejectsNonPositiveMaxRows: P2-OPS-1 — MaxRows <= 0 не
// значит «без лимита» (в отличие от GOTCHA_DIST_RATE_PER_MIN/
// *_RETENTION_DAYS): worker.go гасит собственный потолок условием "> 0", а
// source_events.go всё равно шлёт в ClickHouse LIMIT eventStreamSafetyLimit
// — поток обрывается на миллионе строк, а Truncated остаётся false. Оператор,
// следующий конвенции «0 = без лимита», получал бы тихо обрезанную выгрузку.
func TestConfigValidateRejectsNonPositiveMaxRows(t *testing.T) {
	for _, maxRows := range []int64{0, -1} {
		cfg := validExportConfig()
		cfg.MaxRows = maxRows
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("MaxRows=%d — ожидали ошибку конфигурации", maxRows)
		}
		if !strings.Contains(err.Error(), "MaxRows") {
			t.Errorf("MaxRows=%d: ошибка %q не про MaxRows", maxRows, err)
		}
	}
}

// TestConfigValidateRejectsNonPositiveMaxBytes: та же дыра, что у MaxRows,
// со стороны байтового потолка (worker.go: "MaxBytes > 0 && ...").
func TestConfigValidateRejectsNonPositiveMaxBytes(t *testing.T) {
	for _, maxBytes := range []int64{0, -1} {
		cfg := validExportConfig()
		cfg.MaxBytes = maxBytes
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("MaxBytes=%d — ожидали ошибку конфигурации", maxBytes)
		}
		if !strings.Contains(err.Error(), "MaxBytes") {
			t.Errorf("MaxBytes=%d: ошибка %q не про MaxBytes", maxBytes, err)
		}
	}
}

// TestConfigValidateRejectsNonPositiveDiskBudget: P2-OPS-2 —
// DISK_BUDGET_BYTES<=0 делает "used >= budget" истинным на пустом каталоге:
// каждая заявка отказывает без единой попытки (failPermanent), а не
// «работает без ограничения».
func TestConfigValidateRejectsNonPositiveDiskBudget(t *testing.T) {
	for _, budget := range []int64{0, -1} {
		cfg := validExportConfig()
		cfg.DiskBudget = budget
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("DiskBudget=%d — ожидали ошибку конфигурации", budget)
		}
		if !strings.Contains(err.Error(), "DiskBudget") {
			t.Errorf("DiskBudget=%d: ошибка %q не про DiskBudget", budget, err)
		}
	}
}

// TestConfigValidateRejectsNonPositiveTTL: P2-OPS-2 — TTL_HOURS<=0 делает
// expires_at не позже now(): ближайший тик джанитора сносит только что
// собранный файл, хотя заявка отчиталась успехом и письмо уже ушло.
func TestConfigValidateRejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Hour} {
		cfg := validExportConfig()
		cfg.TTL = ttl
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("TTL=%s — ожидали ошибку конфигурации", ttl)
		}
		if !strings.Contains(err.Error(), "TTL") {
			t.Errorf("TTL=%s: ошибка %q не про TTL", ttl, err)
		}
	}
}

// TestKnownFailureReasonKeyWhitelistsOnlyTheThreeReasons — P2-UX-2 аудита:
// веб-слой сверяет failure_reason_key из БД по этой функции перед i18n.T(),
// потому что i18n.T() на неизвестном ключе возвращает сам ключ как есть, а
// не перевод — без сверки повреждённая/устаревшая строка стала бы
// техническим идентификатором на экране пользователя. Мутация — вернуть
// true по умолчанию (убрать default: return false) — обязана уронить
// случаи "неизвестный ключ" и "пусто" ниже.
func TestKnownFailureReasonKeyWhitelistsOnlyTheThreeReasons(t *testing.T) {
	for _, key := range []string{reasonDiskFull, reasonTooManyGroups, reasonInternal} {
		if !KnownFailureReasonKey(key) {
			t.Errorf("KnownFailureReasonKey(%q) = false, ожидали true", key)
		}
	}
	for _, key := range []string{"", "exports.mail.failed.reason.unknown", "last_error technical text", "exports.mail.done.subject"} {
		if KnownFailureReasonKey(key) {
			t.Errorf("KnownFailureReasonKey(%q) = true, ожидали false", key)
		}
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
