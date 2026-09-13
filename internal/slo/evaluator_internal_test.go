package slo

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// отдаёт фиксированный набор корзин независимо от from/to/step — burnWindows
// сам вычисляет long/short из того, что вернул Provider.
type fixedBucketsProvider struct{ bs []Bucket }

func (p fixedBucketsProvider) Buckets(context.Context, SLO, time.Time, time.Time, time.Duration) ([]Bucket, error) {
	return p.bs, nil
}

func (p fixedBucketsProvider) BucketsExcluding(context.Context, SLO, time.Time, time.Time, time.Duration, []uptime.Window) ([]Bucket, error) {
	return p.bs, nil
}

func (fixedBucketsProvider) RetentionCap() time.Duration { return 0 }

// K66: дырка в последних shortMin минутах не должна тихо растягивать короткое
// окно в прошлое — short обязан остаться пустым, а не откатиться к старой корзине.
func TestSLOEvaluatorBurnWindowsShortIsRecentNotLastSurvivor(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s := SLO{BurnLongMin: 60, BurnShortMin: 5}

	p := fixedBucketsProvider{bs: []Bucket{
		{T: now.Add(-55 * time.Minute), Good: 90, Total: 100},
		{T: now.Add(-10 * time.Minute), Good: 1, Total: 100}, // последняя выжившая, но старше 5 минут
	}}
	e := &Evaluator{}
	long, short, err := e.burnWindows(context.Background(), p, s, now)
	if err != nil {
		t.Fatalf("burnWindows: %v", err)
	}
	if len(long) != 2 {
		t.Fatalf("long = %v, want весь ряд из 2 корзин", long)
	}
	if len(short) != 0 {
		t.Fatalf("short = %v, дырка в последних 5 минутах должна давать пустое окно, а не старую корзину", short)
	}

	fresh := fixedBucketsProvider{bs: []Bucket{
		{T: now.Add(-55 * time.Minute), Good: 90, Total: 100},
		{T: now.Add(-2 * time.Minute), Good: 1, Total: 100}, // внутри последних 5 минут
	}}
	long2, short2, err := e.burnWindows(context.Background(), fresh, s, now)
	if err != nil {
		t.Fatalf("burnWindows: %v", err)
	}
	if len(long2) != 2 {
		t.Fatalf("long2 = %v, want весь ряд из 2 корзин", long2)
	}
	if len(short2) != 1 || short2[0].T != now.Add(-2*time.Minute) {
		t.Fatalf("short2 = %v, want последнюю корзину внутри 5 минут", short2)
	}
}

// T — начало интервала корзины, не момент последних данных в ней: свежесть
// решает конец интервала (T+step) против начала окна (now-step), т.е. строго
// T > now-2*step. Оба граничных числа даны ревьюером буквально.
func TestSLOEvaluatorRecentBucketsBoundary(t *testing.T) {
	now := time.Date(2026, 9, 12, 9, 5, 1, 0, time.UTC)
	step := 5 * time.Minute

	// интервал корзины [09:00:00, 09:05:00) пересекает окно свежести
	// [09:00:01, 09:05:01) — начало корзины вне окна, но данные в ней могут быть
	// не старше пары секунд. Должна войти в short.
	overlapping := []Bucket{{T: now.Add(-step).Add(-time.Second), Good: 1, Total: 1}}
	if got := recentBuckets(overlapping, now, step); len(got) != 1 {
		t.Fatalf("recentBuckets(T=now-step-1s) = %v, корзина пересекает окно и обязана войти в short", got)
	}

	// K66-граница: интервал корзины [08:55:00, 09:00:00) кончается РОВНО на
	// начале окна свежести — самые свежие данные в ней пятиминутной давности.
	// Не должна войти: нестрогое >= вернёт растянутое «короткое» окно (K66).
	stale := []Bucket{{T: now.Add(-2 * step), Good: 1, Total: 1}}
	if got := recentBuckets(stale, now, step); len(got) != 0 {
		t.Fatalf("recentBuckets(T=now-2*step) = %v, конец корзины на границе окна не свежесть — K66 не должен вернуться", got)
	}
}

// tickBudget неэкспортирован — файл живёт в package slo, а не slo_test, как остальные.
func TestSLOEvaluatorTickBudget(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"короткий интервал упирается в пол", time.Second, minTickBudget},
		{"интервал ровно на полу", minTickBudget, minTickBudget},
		{"минута сверх пола считается долей", 60 * time.Second, 48 * time.Second},
		{"не задан — дефолт SLO-интервала (2 минуты) даёт 96s", 0, 96 * time.Second},
		{"отрицательный трактуется как не заданный", -time.Second, 96 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Evaluator{Interval: tc.interval}
			if got := e.tickBudget(); got != tc.want {
				t.Errorf("tickBudget() с Interval=%v = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

func TestRotateSLOs(t *testing.T) {
	slos := []SLO{{ID: 1}, {ID: 2}, {ID: 3}}

	if got := rotateSLOs(nil, 0); got != nil {
		t.Errorf("rotateSLOs(nil) = %v, want nil", got)
	}

	cases := []struct {
		name   string
		cursor int64
		want   []int64
	}{
		{"курсор нулевой — обход с начала", 0, []int64{1, 2, 3}},
		{"курсор на первом — начинаем со второго", 1, []int64{2, 3, 1}},
		{"курсор на среднем", 2, []int64{3, 1, 2}},
		{"курсор на последнем — полный круг", 3, []int64{1, 2, 3}},
		{"курсор за пределами списка (SLO удалён) — оборачиваем как после последнего", 99, []int64{1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rotateSLOs(slos, tc.cursor)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Errorf("rotateSLOs(%v)[%d].ID = %d, want %d", tc.cursor, i, got[i].ID, id)
				}
			}
		})
	}
}

// Сквозной вариант предыдущего теста: реальный Tick(), реальный удалённый SLO —
// проверяет не только саму функцию, но и то, что Tick действительно её зовёт.
func TestTickPrunesCloseStreakOfDeletedSLO(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID, pid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('slo-prune-test', 'SLO Prune Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'slo-prune-test', 'SLO Prune Test') RETURNING id", orgID).
		Scan(&pid); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	st := NewStore(pool)
	kept, err := st.Create(ctx, SLO{
		ProjectID: pid, Name: "kept", Kind: SLIAvailability, Target: 0.99, WindowDays: 30,
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create kept SLO: %v", err)
	}
	deleted, err := st.Create(ctx, SLO{
		ProjectID: pid, Name: "deleted", Kind: SLIAvailability, Target: 0.99, WindowDays: 30,
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create deleted SLO: %v", err)
	}

	// Providers пуст, Tick() трогает только pruneCloseStreak — предзаполняем
	// сами, как будто оба SLO уже копили счётчик остывания.
	e := &Evaluator{Pool: pool, Store: st, Interval: time.Hour}
	e.closeStreak = map[int64]int{kept.ID: 2, deleted.ID: 1}

	if err := st.Delete(ctx, pid, deleted.ID); err != nil {
		t.Fatalf("delete SLO: %v", err)
	}

	if _, err := e.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, ok := e.closeStreak[deleted.ID]; ok {
		t.Errorf("closeStreak[%d] пережил Tick после удаления SLO", deleted.ID)
	}
	if got := e.closeStreak[kept.ID]; got != 2 {
		t.Errorf("closeStreak[%d] = %d, want 2 (живой SLO не должен терять счётчик)", kept.ID, got)
	}
}

// Выключенный/удалённый SLO не должен оставлять запись в closeStreak навсегда.
func TestPruneCloseStreakDropsInactiveSLOs(t *testing.T) {
	e := &Evaluator{closeStreak: map[int64]int{1: 2, 2: 3, 3: 1}}
	e.pruneCloseStreak([]SLO{{ID: 1}, {ID: 3}})

	if _, ok := e.closeStreak[2]; ok {
		t.Errorf("closeStreak[2] пережил pruneCloseStreak, хотя SLO 2 не в активном списке")
	}
	if got := e.closeStreak[1]; got != 2 {
		t.Errorf("closeStreak[1] = %d, want 2 (активный SLO не должен терять счётчик)", got)
	}
	if got := e.closeStreak[3]; got != 1 {
		t.Errorf("closeStreak[3] = %d, want 1 (активный SLO не должен терять счётчик)", got)
	}
}
