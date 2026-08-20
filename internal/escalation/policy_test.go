package escalation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newProject: прямые вставки — escalation-пакет не зависит от org. slug
// организации/проекта фиксирован ('escorg'/'esc') — годится, пока тест
// заводит ровно один проект; тест с двумя проектами (cross-tenant) должен
// звать newProjectNamed с разными слагами, иначе второй INSERT упадёт на
// organizations_slug_key.
func newProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	return newProjectNamed(t, pool, "escorg", "esc")
}

// newProjectNamed — та же прямая вставка, но с явными слагами: нужно тестам,
// заводящим больше одного проекта в рамках одного pool (см. newProject).
func newProjectNamed(t *testing.T, pool *pgxpool.Pool, orgSlug, projectSlug string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Esc Org',1000000) RETURNING id", orgSlug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,'Esc') RETURNING id", orgID, projectSlug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func newChannel(t *testing.T, pool *pgxpool.Pool, projectID int64, enabled bool) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO alert_channels (project_id, kind, enabled, target) VALUES ($1,'email',$2,'a@b.c') RETURNING id",
		projectID, enabled).Scan(&id); err != nil {
		t.Fatalf("channel: %v", err)
	}
	return id
}

func ladderEqual(a, b escalation.Ladder) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].StepNo != b[i].StepNo || a[i].DelayMinutes != b[i].DelayMinutes {
			return false
		}
		if len(a[i].ChannelIDs) != len(b[i].ChannelIDs) {
			return false
		}
		for j := range a[i].ChannelIDs {
			if a[i].ChannelIDs[j] != b[i].ChannelIDs[j] {
				return false
			}
		}
	}
	return true
}

// TestLadderDefaultFallback — дискриминирующий тест BLOCKER-1: проект с двумя
// enabled-каналами и одним disabled, без настроенной политики, обязан
// получить дефолт-лесенку из ОДНОЙ ступени delay0=0 с ДВУМЯ enabled-каналами
// (disabled в неё не входит). Ровно старое поведение до эскалаций.
func TestLadderDefaultFallback(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	_ = newChannel(t, pool, pid, false) // disabled — не должен попасть в дефолт

	ladder, err := store.Ladder(ctx, pid, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	want := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	if !ladderEqual(ladder, want) {
		t.Fatalf("Ladder = %+v, want %+v", ladder, want)
	}
}

// TestLadderConfigured — SetLadder сохраняет лесенку и Ladder возвращает её
// отсортированной по step_no; другая severity того же проекта, для которой
// политика не настраивалась, по-прежнему получает дефолт-fallback (не
// пустую лесенку).
func TestLadderConfigured(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	c3 := newChannel(t, pool, pid, true)

	steps := []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		{StepNo: 1, DelayMinutes: 15, ChannelIDs: []int64{c2, c3}},
	}
	if err := store.SetLadder(ctx, pid, escalation.SeverityCritical, steps); err != nil {
		t.Fatalf("SetLadder: %v", err)
	}

	ladder, err := store.Ladder(ctx, pid, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	want := escalation.Ladder{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		{StepNo: 1, DelayMinutes: 15, ChannelIDs: []int64{c2, c3}},
	}
	if !ladderEqual(ladder, want) {
		t.Fatalf("Ladder(critical) = %+v, want %+v", ladder, want)
	}

	// warning для того же проекта не настраивалась — дефолт-fallback, а не пусто.
	warnLadder, err := store.Ladder(ctx, pid, escalation.SeverityWarning)
	if err != nil {
		t.Fatalf("Ladder(warning): %v", err)
	}
	wantWarn := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2, c3}}}
	if !ladderEqual(warnLadder, wantWarn) {
		t.Fatalf("Ladder(warning) = %+v, want default fallback %+v", warnLadder, wantWarn)
	}
}

// TestSetLadderReplaces — второй вызов SetLadder затирает первую лесенку
// целиком, без дублей и без утечки старых ступеней/каналов.
func TestSetLadderReplaces(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)

	if err := store.SetLadder(ctx, pid, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		{StepNo: 1, DelayMinutes: 10, ChannelIDs: []int64{c2}},
	}); err != nil {
		t.Fatalf("SetLadder (1st): %v", err)
	}
	if err := store.SetLadder(ctx, pid, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 5, ChannelIDs: []int64{c2}},
	}); err != nil {
		t.Fatalf("SetLadder (2nd): %v", err)
	}

	ladder, err := store.Ladder(ctx, pid, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	want := escalation.Ladder{{StepNo: 0, DelayMinutes: 5, ChannelIDs: []int64{c2}}}
	if !ladderEqual(ladder, want) {
		t.Fatalf("Ladder after replace = %+v, want %+v", ladder, want)
	}

	var stepCount int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM escalation_steps WHERE project_id=$1 AND severity='critical'", pid).Scan(&stepCount); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if stepCount != 1 {
		t.Fatalf("escalation_steps count = %d, want 1 (no leftover from 1st call)", stepCount)
	}
}

func TestSetLadderValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	cases := []struct {
		name     string
		severity string
		steps    []escalation.Step
	}{
		{"gap in step_no", escalation.SeverityCritical, []escalation.Step{
			{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
			{StepNo: 2, DelayMinutes: 10, ChannelIDs: []int64{c1}},
		}},
		{"negative delay", escalation.SeverityCritical, []escalation.Step{
			{StepNo: 0, DelayMinutes: -1, ChannelIDs: []int64{c1}},
		}},
		{"invalid severity", "urgent", []escalation.Step{
			{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		}},
		{"missing step 0", escalation.SeverityCritical, []escalation.Step{
			{StepNo: 1, DelayMinutes: 10, ChannelIDs: []int64{c1}},
		}},
		{"step without channels", escalation.SeverityCritical, []escalation.Step{
			{StepNo: 0, DelayMinutes: 0, ChannelIDs: nil},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetLadder(ctx, pid, tc.severity, tc.steps); err == nil {
				t.Fatalf("SetLadder(%s): want error, got nil", tc.name)
			} else if !errors.Is(err, escalation.ErrInvalidPolicy) {
				t.Fatalf("SetLadder(%s): err = %v, want wrapping ErrInvalidPolicy", tc.name, err)
			}
		})
	}
}

// TestSetLadderForeignChannel — cross-tenant (T9, concern T2): channel_id
// принадлежащий ДРУГОМУ проекту отвергается ДО любой записи — ни новые шаги,
// ни их каналы не должны попасть в БД (транзакция откатывается целиком, а не
// только для чужого id).
func TestSetLadderForeignChannel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pidA := newProjectNamed(t, pool, "escorg-a", "esc-a")
	pidB := newProjectNamed(t, pool, "escorg-b", "esc-b")
	ownChannel := newChannel(t, pool, pidA, true)
	foreignChannel := newChannel(t, pool, pidB, true)

	err := store.SetLadder(ctx, pidA, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{ownChannel, foreignChannel}},
	})
	if err == nil {
		t.Fatalf("SetLadder with foreign channel: want error, got nil")
	}
	if !errors.Is(err, escalation.ErrInvalidPolicy) {
		t.Fatalf("SetLadder with foreign channel: err = %v, want wrapping ErrInvalidPolicy", err)
	}

	var stepCount int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM escalation_steps WHERE project_id=$1", pidA).Scan(&stepCount); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if stepCount != 0 {
		t.Fatalf("escalation_steps count after rejected SetLadder = %d, want 0 (nothing persisted)", stepCount)
	}
}

// TestLadderNoChannelsNoPolicy — проект без единого канала и без настроенной
// политики получает дефолт-лесенку с пустым ChannelIDs, а не панику/ошибку.
func TestLadderNoChannelsNoPolicy(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	ladder, err := store.Ladder(ctx, pid, escalation.SeverityWarning)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(ladder) != 1 || ladder[0].StepNo != 0 || ladder[0].DelayMinutes != 0 || len(ladder[0].ChannelIDs) != 0 {
		t.Fatalf("Ladder = %+v, want single step0 delay0 with empty channels", ladder)
	}
}

// TestLadders — Ladders возвращает обе severity, дефолт-fallback для той, что
// не настраивалась.
func TestLadders(t *testing.T) {
	pool := testenv.MigratedPG(t)
	store := escalation.NewPolicyStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	if err := store.SetLadder(ctx, pid, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	}); err != nil {
		t.Fatalf("SetLadder: %v", err)
	}

	ladders, err := store.Ladders(ctx, pid)
	if err != nil {
		t.Fatalf("Ladders: %v", err)
	}
	if len(ladders) != 2 {
		t.Fatalf("Ladders = %+v, want 2 severities", ladders)
	}
	if !ladderEqual(ladders[escalation.SeverityCritical], escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}) {
		t.Fatalf("Ladders[critical] = %+v", ladders[escalation.SeverityCritical])
	}
	if !ladderEqual(ladders[escalation.SeverityWarning], escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}) {
		t.Fatalf("Ladders[warning] (default fallback) = %+v", ladders[escalation.SeverityWarning])
	}
}
