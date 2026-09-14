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

type fakeIssueSource struct {
	n       int
	failAt  int // индекс записи, на которой вернуть failErr вместо записи
	failErr error
	seenPII []bool
}

func fakeIssues(n int) IssueSource { return &fakeIssueSource{n: n} }

func failingSource(err error) IssueSource {
	return &fakeIssueSource{n: 1, failAt: 0, failErr: err}
}

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
	if j.Status != StatusQueued || j.Attempts != 1 {
		t.Fatalf("временный отказ обработан как окончательный: %+v", j)
	}
}

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

	j, getErr := st.Get(ctx, id)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if j.Status != StatusQueued || j.Attempts != 0 {
		t.Fatalf("заявку тронули при неверной конфигурации: %+v", j)
	}
}

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

type permanentFailAfterShutdownSource struct {
	cancel  context.CancelFunc
	permErr error
}

func (s *permanentFailAfterShutdownSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
	s.cancel()
	return s.permErr
}

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
		if j.ExpiresAt == nil {
			t.Fatal("Notify получил заявку без ExpiresAt — письмо не сможет назвать срок удаления файла")
		}
		// Сверяем с ЗАПИСАННЫМ значением, не с диапазоном: независимый пересчёт по тому же
		// TTL прошёл бы любую проверку "около часа", но разошёлся бы с базой на микросекунды.
		got, err := st.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ExpiresAt == nil || !j.ExpiresAt.Equal(*got.ExpiresAt) {
			t.Errorf("Notify получил ExpiresAt %v, в базе записано %v — не одно и то же значение", j.ExpiresAt, got.ExpiresAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run не обработал заявку по тикеру за отведённое время")
	}
}

func TestWorkerRunStopsOnContextCancel(t *testing.T) {
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
	if defaultJobTimeout >= leaseTTL {
		t.Fatalf("defaultJobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", defaultJobTimeout, leaseTTL)
	}
}

func TestConfigValidateRejectsJobTimeoutAtOrAboveLeaseTTL(t *testing.T) {
	cfg := validExportConfig()
	cfg.JobTimeout = leaseTTL
	if err := cfg.Validate(); err == nil {
		t.Fatal("JobTimeout == leaseTTL — ожидали ошибку конфигурации")
	}
}

func validExportConfig() Config {
	return Config{
		MaxRows:    1000,
		MaxBytes:   1 << 20,
		DiskBudget: 1 << 30,
		TTL:        time.Hour,
	}
}

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

type raceDirEntry struct {
	os.DirEntry
	path string
}

func (r raceDirEntry) Info() (os.FileInfo, error) {
	if err := os.Remove(r.path); err != nil {
		return nil, err
	}
	return r.DirEntry.Info()
}

func TestSizeOfEntriesSkipsFileRemovedBetweenReadDirAndInfo(t *testing.T) {
	dir := t.TempDir()
	keepPath := filepath.Join(dir, "keep.csv")
	vanishPath := filepath.Join(dir, "vanish.csv")
	if err := os.WriteFile(keepPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	if err := os.WriteFile(vanishPath, []byte("этот файл исчезнет до Info()"), 0o600); err != nil {
		t.Fatalf("write vanish: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for i, e := range entries {
		if e.Name() == "vanish.csv" {
			entries[i] = raceDirEntry{DirEntry: e, path: vanishPath}
			found = true
		}
	}
	if !found {
		t.Fatalf("vanish.csv не попал в список записей каталога — тест не воспроизводит гонку")
	}

	got, err := sizeOfEntries(entries)
	if err != nil {
		t.Fatalf("sizeOfEntries: удалённый параллельным джанитором файл не должен валить подсчёт: %v", err)
	}
	if want := int64(len("0123456789")); got != want {
		t.Fatalf("total=%d, want %d (учтён только keep.csv, vanish.csv уже удалён к моменту Stat)", got, want)
	}
	if _, err := os.Stat(vanishPath); !os.IsNotExist(err) {
		t.Fatalf("vanish.csv должен быть реально удалён к этому моменту — иначе гонка не воспроизведена")
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
