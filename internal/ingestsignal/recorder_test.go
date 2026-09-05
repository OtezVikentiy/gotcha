package ingestsignal_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRecorderTouchFlushWritesOnce — 5 Touch одной пары + 1 Touch другой →
// Flush пишет ДВЕ строки с hits 5 и 1; повторный Flush без новых Touch ничего
// не меняет (pending уже пуст после первого Flush).
func TestRecorderTouchFlushWritesOnce(t *testing.T) {
	st, pid := setupProject(t)
	r := ingestsignal.NewRecorder(st)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		r.Touch(pid, ingestsignal.KindKeyInvalid)
	}
	r.Touch(pid, ingestsignal.KindKeyScope)

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	hits := signalHits(t, st, pid)
	if hits[ingestsignal.KindKeyInvalid] != 5 {
		t.Errorf("hits[key_invalid] = %d, want 5", hits[ingestsignal.KindKeyInvalid])
	}
	if hits[ingestsignal.KindKeyScope] != 1 {
		t.Errorf("hits[key_scope] = %d, want 1", hits[ingestsignal.KindKeyScope])
	}

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	again := signalHits(t, st, pid)
	if again[ingestsignal.KindKeyInvalid] != 5 || again[ingestsignal.KindKeyScope] != 1 {
		t.Errorf("после повторного Flush без Touch = %+v, want без изменений (5 и 1)", again)
	}
}

// TestRecorderMaxPendingDrops — MaxPending=2, три РАЗНЫЕ пары Touch: третья
// (сверх потолка) не должна дойти до Flush.
func TestRecorderMaxPendingDrops(t *testing.T) {
	st, pid := setupProject(t)
	r := ingestsignal.NewRecorder(st)
	r.MaxPending = 2
	ctx := context.Background()

	r.Touch(pid, ingestsignal.KindKeyInvalid)
	r.Touch(pid, ingestsignal.KindKeyScope)
	r.Touch(pid, ingestsignal.KindKeyProjectMismatch) // сверх потолка — отброшена

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("строк в БД %d, want 2 (потолок MaxPending=2): %+v", len(got), got)
	}
}

// TestRecorderRunFlushesOnCancel — Touch, затем отмена ctx: Run обязан
// сделать финальный Flush ПЕРЕД возвратом, а не бросить накопленное.
func TestRecorderRunFlushesOnCancel(t *testing.T) {
	st, pid := setupProject(t)
	r := ingestsignal.NewRecorder(st)
	r.FlushEvery = time.Hour // тик не должен успеть сработать сам за время теста
	ctx, cancel := context.WithCancel(context.Background())

	r.Touch(pid, ingestsignal.KindKeyInvalid)

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены ctx (финальный Flush завис)")
	}

	got := signalHits(t, st, pid)
	if got[ingestsignal.KindKeyInvalid] != 1 {
		t.Fatalf("hits[key_invalid] = %d, want 1 (финальный Flush на cancel)", got[ingestsignal.KindKeyInvalid])
	}
}

// TestRecorderRunFlushesOnTick — Touch, затем НЕ отменяя ctx: Run обязан сам
// флашить по тику FlushEvery, а не только на остановке (мутация M10: тик без
// флаша выживал, потому что ни один тест не ждал именно тика).
func TestRecorderRunFlushesOnTick(t *testing.T) {
	st, pid := setupProject(t)
	r := ingestsignal.NewRecorder(st)
	r.FlushEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	r.Touch(pid, ingestsignal.KindKeyInvalid)

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := signalHits(t, st, pid)
		if got[ingestsignal.KindKeyInvalid] == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("тик не флашнул Touch за 2с: hits[key_invalid] = %d, want 1", got[ingestsignal.KindKeyInvalid])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRecorderFlushJoinsErrorsForAllPairs — minor m1: Flush не должен
// останавливаться на первой же ошибке Bump — errors.Join обязан собрать ВСЕ
// ошибки, иначе одна упавшая пара молча съедала бы остальные (мутация «return
// err на первой ошибке» вместо накопления).
func TestRecorderFlushJoinsErrorsForAllPairs(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := ingestsignal.NewStore(pool)
	r := ingestsignal.NewRecorder(st)
	r.Touch(1, ingestsignal.KindKeyInvalid)
	r.Touch(2, ingestsignal.KindKeyScope)
	pool.Close()

	err := r.Flush(context.Background())
	if err == nil {
		t.Fatal("Flush на закрытом пуле не вернул ошибку")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Flush error = %T, want errors.Join (обёртка над обеими ошибками Bump)", err)
	}
	if got := len(joined.Unwrap()); got != 2 {
		t.Errorf("errors.Join хранит %d ошибок, want 2 — обе пары обязаны попытаться Bump, а не остановиться на первой", got)
	}
}

// TestRecorderRunDefaultsZeroFlushEvery — minor m1: FlushEvery<=0 (обнулён
// после NewRecorder) не должен уйти в NewTicker(0) (паника) — Run обязан
// подставить defaultFlushEvery.
func TestRecorderRunDefaultsZeroFlushEvery(t *testing.T) {
	st, pid := setupProject(t)
	r := ingestsignal.NewRecorder(st)
	r.FlushEvery = 0
	ctx, cancel := context.WithCancel(context.Background())

	r.Touch(pid, ingestsignal.KindKeyInvalid)

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился — похоже, FlushEvery<=0 ушёл в NewTicker(0) без дефолта")
	}

	got := signalHits(t, st, pid)
	if got[ingestsignal.KindKeyInvalid] != 1 {
		t.Fatalf("hits[key_invalid] = %d, want 1 (финальный Flush обязан отработать несмотря на FlushEvery=0)", got[ingestsignal.KindKeyInvalid])
	}
}

// signalHits — сигналы проекта как map kind→hits, для удобства сравнения в
// тестах Recorder.
func signalHits(t *testing.T, st *ingestsignal.Store, projectID int64) map[ingestsignal.Kind]int64 {
	t.Helper()
	got, err := st.ForProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	out := make(map[ingestsignal.Kind]int64, len(got))
	for _, s := range got {
		out[s.Kind] = s.Hits
	}
	return out
}
