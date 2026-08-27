package export

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// TestJanitorSkipsWhenAnotherInstanceHoldsLock — второй экземпляр джанитора
// не должен начинать чистку параллельно: проверка бьёт по самому механизму
// exclusivity (advisory lock), держа его на отдельном соединении, как это
// делала бы соседняя реплика. По образцу
// TestWorkerSkipsWhenAnotherInstanceHoldsLock (worker_test.go).
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

// TestJanitorOrphanNameParsingIsStrict — разбор имени файла в removeOrphans
// обязан быть строгим: только "<положительное целое>.<расширение>" считается
// кандидатом в сироты. Всё остальное — чужие файлы, которые джанитор не
// вправе трогать, даже если внешне похожи на его формат.
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
