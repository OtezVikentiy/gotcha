package export

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestJanitorRemovesExpiredFile(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("id,title\n1,x\n"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: 30 * 24 * time.Hour}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("файл с истёкшим сроком остался на диске")
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusExpired {
		t.Errorf("статус после уборки %q, ожидали expired", j.Status)
	}
}

func TestJanitorKeepsLiveFile(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("id,title\n1,x\n"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: 30 * 24 * time.Hour}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("живой файл убран раньше срока: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone {
		t.Errorf("статус живой заявки изменился на %q", j.Status)
	}
}

func TestJanitorRemovesOrphanFiles(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)

	// Живая заявка — контроль: сироты не должны её задеть.
	liveID := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() + interval '1 hour' WHERE id = $1`, liveID); err != nil {
		t.Fatalf("подготовка живой заявки: %v", err)
	}
	liveFile := filepath.Join(dir, fmt.Sprintf("%d.csv", liveID))
	if err := os.WriteFile(liveFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись живого файла: %v", err)
	}

	orphan := filepath.Join(dir, "999999.csv")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись файла-сироты: %v", err)
	}
	stale := filepath.Join(dir, "999998.part") // мусор от упавшего инстанса
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись мусорного .part: %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Свежий .part — воркер, вероятно, пишет его прямо сейчас, трогать нельзя.
	fresh := filepath.Join(dir, "999997.part")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись свежего .part: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: 30 * 24 * time.Hour}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	for _, f := range []string{orphan, stale} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("сирота %s не убрана", filepath.Base(f))
		}
	}
	// Файл живой заявки трогать нельзя.
	if _, err := os.Stat(liveFile); err != nil {
		t.Errorf("убран файл живой заявки: %v", err)
	}
	// Свежий .part пока не мусор — гонка с активным воркером.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("убран свежий .part файл: %v", err)
	}
}

func TestJanitorPurgesOldRows(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)

	oldID := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	freshID := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='failed',
		finished_at = now() - interval '40 days' WHERE id = $1`, oldID); err != nil {
		t.Fatalf("подготовка старой заявки: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='failed',
		finished_at = now() - interval '1 hour' WHERE id = $1`, freshID); err != nil {
		t.Fatalf("подготовка свежей заявки: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: 30 * 24 * time.Hour}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, err := st.Get(ctx, oldID); err != ErrNotFound {
		t.Errorf("старая строка не вычищена: err=%v", err)
	}
	if _, err := st.Get(ctx, freshID); err != nil {
		t.Errorf("свежая строка ошибочно удалена: %v", err)
	}
}

func TestJanitorSkipsWhenAnotherInstanceHoldsLock(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)

	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(janitorLockKey)).Scan(&locked); err != nil || !locked {
		t.Fatalf("подготовка занятого лока: locked=%v err=%v", locked, err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(janitorLockKey))

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, err := os.Stat(file); err != nil {
		t.Errorf("файл истёкшей заявки убран, пока лок держала другая реплика: %v", err)
	}
	j, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != StatusDone {
		t.Errorf("статус заявки изменён, пока лок держала другая реплика: %q", j.Status)
	}
}

func TestJanitorOrphanNameParsingIsStrict(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()

	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("запись %s: %v", name, err)
		}
		return p
	}

	// Валидный сирота (строки в export_jobs с таким id нет вовсе) — обязан
	// быть убран.
	orphan := write("42.csv")

	// Не числовой base — не наш файл.
	nonNumeric := write("report.csv")
	// Ноль и отрицательное число — валидный ParseInt, но не валидный id.
	zero := write("0.csv")
	negative := write("-5.csv")
	// Без расширения — не формат "<id>.<ext>".
	noExt := write("noext")

	// Поддиректория с "похожим" на файл именем — не файл вовсе, IsDir режет
	// её раньше разбора имени.
	subdir := filepath.Join(dir, "7.csv.d")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour}
	if err := jan.removeOrphans(ctx); err != nil {
		t.Fatalf("removeOrphans: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("валидный сирота не убран")
	}
	for _, f := range []string{nonNumeric, zero, negative, noExt} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("файл вне строгого формата <id>.<ext> ошибочно тронут: %s: %v", filepath.Base(f), err)
		}
	}
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("поддиректория ошибочно тронута: %v", err)
	}
}

func TestJanitorRunStopsOnCancel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour, Interval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		jan.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Janitor.Run не завершился после отмены контекста")
	}
}

func TestJanitorRunFirstPassIsImmediate(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("id,title\n1,x\n"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	// Interval заведомо больше времени теста — если бы первого прохода не
	// было, файл дожил бы до конца теста нетронутым.
	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: 30 * 24 * time.Hour, Interval: time.Hour}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go jan.Run(runCtx)

	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("файл с истёкшим сроком не убран за 5с — первого прохода нет, чистка ждёт целый Interval")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestJanitorTickRecordsLastTick(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour}
	if got := jan.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix() до первого Tick = %d, want 0", got)
	}
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jan.LastTickUnix(); got == 0 {
		t.Errorf("LastTickUnix() после Tick = 0, want > 0")
	}
	if got := jan.LastTickSeconds(); got < 0 || got >= 5 {
		t.Errorf("LastTickSeconds() = %v, want в диапазоне [0, 5) на пустой БД", got)
	}
}

func TestJanitorTickBudget(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"ниже пола — берём пол", time.Second, minTickBudget},
		{"выше пола — доля Interval", 60 * time.Second, 48 * time.Second},
		{"нулевой Interval — доля дефолта, как у Run", 0, time.Duration(float64(defaultJanitorInterval) * tickBudgetShare)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jan := &Janitor{Interval: tt.interval}
			if got := jan.tickBudget(); got != tt.want {
				t.Errorf("tickBudget() при Interval=%v = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}

func TestJanitorUnlockSurvivesCancelledContext(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)

	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	// Триггер держит любой UPDATE export_jobs секундной паузой — ровно тот
	// момент, когда expireDue вызывает Store.MarkExpired.
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_delay_export_jobs_update()
		RETURNS trigger AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("создание функции задержки: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_delay_export_jobs_update
		BEFORE UPDATE ON export_jobs FOR EACH ROW
		EXECUTE FUNCTION test_delay_export_jobs_update()`); err != nil {
		t.Fatalf("создание триггера: %v", err)
	}

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tctx, cancel := context.WithCancel(ctx)
	tickDone := make(chan error, 1)
	go func() { tickDone <- jan.Tick(tctx) }()

	// 200мс — далеко внутри секундной паузы триггера, с большим запасом в обе стороны.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-tickDone:
		if err == nil {
			t.Fatal("Tick с отменённым посреди expireDue ctx вернул nil — отмена не долетела до паузы триггера")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Tick не вернулся после отмены ctx — завис")
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER test_delay_export_jobs_update ON export_jobs`); err != nil {
		t.Fatalf("снятие триггера: %v", err)
	}

	if strings.Contains(logBuf.String(), "снятие advisory lock") {
		t.Errorf("снятие advisory lock залогировало WARN при отменённом ctx: %s", logBuf.String())
	}

	// Если первый Tick лок не снял, второй тихо выйдет через ветку !locked, и файл останется.
	if err := jan.Tick(ctx); err != nil {
		t.Fatalf("Tick с живым ctx: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("файл истёкшей заявки не убран вторым Tick'ом — похоже, лок после первого Tick не снят: stat err=%v", err)
	}
}

func TestJanitorTickBudgetAbortsHungTick(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	dir := t.TempDir()
	projectID, userID := seedProjectAndUser(t, pool)

	id := mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.csv", id))
	if err := os.WriteFile(file, []byte("id,title\n1,x\n"), 0o600); err != nil {
		t.Fatalf("запись файла: %v", err)
	}

	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_hang_export_jobs_update()
		RETURNS trigger AS $$ BEGIN PERFORM pg_sleep(15); RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("создание функции задержки: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_hang_export_jobs_update
		BEFORE UPDATE ON export_jobs FOR EACH ROW
		EXECUTE FUNCTION test_hang_export_jobs_update()`); err != nil {
		t.Fatalf("создание триггера: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_hang_export_jobs_update ON export_jobs`); err != nil {
			t.Errorf("снятие триггера: %v", err)
		}
	})

	jan := &Janitor{Store: st, Pool: pool, Dir: dir, RowRetention: time.Hour, Interval: time.Second}

	started := time.Now()
	type result struct {
		err error
		dur time.Duration
	}
	tickDone := make(chan result, 1)
	go func() {
		err := jan.Tick(context.Background())
		tickDone <- result{err: err, dur: time.Since(started)}
	}()

	select {
	case r := <-tickDone:
		if r.err == nil {
			t.Fatal("Tick с повисшим триггером (pg_sleep 15s) вернул nil — тик не ограничен бюджетом")
		}
		if r.dur > 12*time.Second {
			t.Errorf("Tick вернулся через %v, want ограничение бюджетом (minTickBudget=10s) — похоже, тик просто дождался паузы триггера", r.dur)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Tick не вернулся за 20s — тик завис на повисшей PG-операции")
	}

	if got := jan.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету тика, want 0", got)
	}
	if got := jan.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}
