package metric_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", t.Name()+"@e.com").Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id", t.Name()+"-o").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) VALUES ($1,$2,$2,'go') RETURNING id", orgID, t.Name()+"-p").Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func TestRuleServiceCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := metric.NewRuleService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	valid := metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true}
	created, err := svc.Create(ctx, valid)
	if err != nil || created.ID == 0 {
		t.Fatalf("create = (%+v,%v)", created, err)
	}
	// Невалидные.
	for _, bad := range []metric.Rule{
		{ProjectID: projectID, MetricName: "", Aggregation: "avg", Comparator: "gt", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "bogus", Comparator: "gt", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "avg", Comparator: "ne", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "avg", Comparator: "gt", WindowSeconds: 0},
	} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, metric.ErrInvalidRule) {
			t.Fatalf("Create(%+v) = %v, want ErrInvalidRule", bad, err)
		}
	}
	// List + ListEnabled.
	if rules, _ := svc.List(ctx, projectID); len(rules) != 1 {
		t.Fatalf("List = %d, want 1", len(rules))
	}
	if enabled, _ := svc.ListEnabled(ctx); len(enabled) != 1 {
		t.Fatalf("ListEnabled = %d, want 1", len(enabled))
	}
	// Delete scoped: чужой projectID не удаляет.
	if err := svc.Delete(ctx, created.ID, projectID+999); err != nil {
		t.Fatalf("delete scoped: %v", err)
	}
	if rules, _ := svc.List(ctx, projectID); len(rules) != 1 {
		t.Fatalf("scoped delete removed rule")
	}
	if err := svc.Delete(ctx, created.ID, projectID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rules, _ := svc.List(ctx, projectID); len(rules) != 0 {
		t.Fatalf("rule not deleted")
	}
}

func TestRuleServiceUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := metric.NewRuleService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	target, err := svc.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	witness, err := svc.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "mem", Aggregation: "max", Comparator: "lt", Threshold: 10, WindowSeconds: 60, Enabled: true})
	if err != nil {
		t.Fatalf("create witness: %v", err)
	}

	// Успешная правка меняет все поля, включая enabled=false, и сохраняет id.
	upd := metric.Rule{
		ID: target.ID, ProjectID: projectID, MetricName: "cpu.load", Aggregation: "p95",
		Comparator: "lt", Threshold: 42.5, WindowSeconds: 600,
		Environment: "prod", LabelKey: "host", LabelValue: "db-1",
		Enabled: false, Severity: "critical",
	}
	got, err := svc.Update(ctx, upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.ID != target.ID || got.MetricName != "cpu.load" || got.Aggregation != "p95" ||
		got.Comparator != "lt" || got.Threshold != 42.5 || got.WindowSeconds != 600 ||
		got.Environment != "prod" || got.LabelKey != "host" || got.LabelValue != "db-1" ||
		got.Enabled || got.Severity != "critical" {
		t.Fatalf("update result = %+v", got)
	}
	// Выключенное правило пропадает из ListEnabled.
	if enabled, _ := svc.ListEnabled(ctx); len(enabled) != 1 || enabled[0].ID != witness.ID {
		t.Fatalf("ListEnabled after disable = %+v, want only witness %d", enabled, witness.ID)
	}
	// Правило-свидетель не тронуто. CreatedAt сравнивается через Equal:
	// прямое == у time.Time чувствительно к представлению, не к моменту.
	w2, found, err := svc.Get(ctx, witness.ID)
	if err != nil || !found {
		t.Fatalf("get witness: (%v,%v)", found, err)
	}
	if !w2.CreatedAt.Equal(witness.CreatedAt) {
		t.Fatalf("witness created_at changed: %v, want %v", w2.CreatedAt, witness.CreatedAt)
	}
	w2.CreatedAt, witness.CreatedAt = time.Time{}, time.Time{}
	if w2 != witness {
		t.Fatalf("witness changed: %+v, want %+v", w2, witness)
	}

	// Чужой projectID → ErrRuleNotFound, правило не меняется.
	foreign := upd
	foreign.ProjectID = projectID + 999
	foreign.MetricName = "hacked"
	if _, err := svc.Update(ctx, foreign); !errors.Is(err, metric.ErrRuleNotFound) {
		t.Fatalf("cross-project update = %v, want ErrRuleNotFound", err)
	}
	if after, _, _ := svc.Get(ctx, target.ID); after.MetricName != "cpu.load" {
		t.Fatalf("cross-project update leaked: %+v", after)
	}
	// Несуществующий id → ErrRuleNotFound.
	missing := upd
	missing.ID = target.ID + 12345
	if _, err := svc.Update(ctx, missing); !errors.Is(err, metric.ErrRuleNotFound) {
		t.Fatalf("missing id update = %v, want ErrRuleNotFound", err)
	}
	// Невалидные поля → ErrInvalidRule (та же валидация, что у Create),
	// отличимый от «не найдено».
	bad := upd
	bad.Aggregation = "bogus"
	if _, err := svc.Update(ctx, bad); !errors.Is(err, metric.ErrInvalidRule) {
		t.Fatalf("invalid update = %v, want ErrInvalidRule", err)
	}
}

// TestRuleServiceUpdateDisableClosesIncident — выключение правила закрывает
// его открытый инцидент в той же транзакции (иначе он завис бы навсегда:
// ListEnabled перестаёт отдавать правило, а планировщик эскалаций продолжал
// бы слать ступени). Свидетель заведён ПЕРВЫМ: мутация, теряющая скоуп
// rule_id в выборке открытого инцидента, детерминированно зацепит его.
func TestRuleServiceUpdateDisableClosesIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	witness, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "mem", Aggregation: "max", Comparator: "gt", Threshold: 80, WindowSeconds: 60, Enabled: true})
	if err != nil {
		t.Fatalf("create witness rule: %v", err)
	}
	target, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create target rule: %v", err)
	}
	inW, _, err := inc.Open(ctx, witness.ID, projectID, 200, false, "")
	if err != nil {
		t.Fatalf("open witness incident: %v", err)
	}
	inT, _, err := inc.Open(ctx, target.ID, projectID, 150, false, "")
	if err != nil {
		t.Fatalf("open target incident: %v", err)
	}

	// Правка БЕЗ выключения открытый инцидент не трогает.
	upd := target
	upd.Threshold = 95
	if _, err := rules.Update(ctx, upd); err != nil {
		t.Fatalf("update enabled: %v", err)
	}
	if got, _, _ := inc.GetByID(ctx, inT.ID); got.Status != "open" || got.ResolvedAt != nil {
		t.Fatalf("enabled update must not touch incident: %+v", got)
	}

	// Выключение закрывает инцидент штатными полями закрытия: resolved +
	// resolved_at, current_value сохранён, notified_close=false (recovery не
	// отправлялось — закрытие молчаливое, восстановления не было).
	upd.Enabled = false
	if _, err := rules.Update(ctx, upd); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, found, err := inc.GetByID(ctx, inT.ID)
	if err != nil || !found {
		t.Fatalf("get target incident: (%v,%v)", found, err)
	}
	if got.Status != "resolved" {
		t.Fatalf("incident must be resolved after rule disable, got status %q", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Fatalf("resolved_at must be set on disable-close: %+v", got)
	}
	if got.CurrentValue != 150 {
		t.Fatalf("current_value must be preserved on disable-close: %v, want 150", got.CurrentValue)
	}
	if got.NotifiedClose {
		t.Fatalf("disable-close must be silent (notified_close=false): %+v", got)
	}
	// Свидетель: инцидент другого правила остался открытым.
	if gotW, _, _ := inc.GetByID(ctx, inW.ID); gotW.Status != "open" || gotW.ResolvedAt != nil {
		t.Fatalf("witness incident touched by disable-close: %+v", gotW)
	}

	// Повторное сохранение УЖЕ выключенного правила подметает его открытый
	// инцидент: такой инцидент недостижим для оценщика по определению
	// (застрявший — например, открыт гонкой с тиком: ListEnabled прочитан до
	// коммита выключения; здесь заводится руками), и повторное сохранение —
	// единственный способ его закрыть. Поля закрытия — штатные, как в
	// основном сценарии.
	inR, _, err := inc.Open(ctx, target.ID, projectID, 170, false, "")
	if err != nil {
		t.Fatalf("open raced incident: %v", err)
	}
	if _, err := rules.Update(ctx, upd); err != nil {
		t.Fatalf("re-save disabled: %v", err)
	}
	gotR, _, err := inc.GetByID(ctx, inR.ID)
	if err != nil {
		t.Fatalf("get raced incident: %v", err)
	}
	if gotR.Status != "resolved" || gotR.ResolvedAt == nil {
		t.Fatalf("re-save of disabled rule must sweep raced incident: %+v", gotR)
	}
	if gotR.CurrentValue != 170 || gotR.NotifiedClose {
		t.Fatalf("sweep must close with regular fields (current preserved, silent): %+v", gotR)
	}
	// Свидетель пережил и подметание.
	if gotW, _, _ := inc.GetByID(ctx, inW.ID); gotW.Status != "open" {
		t.Fatalf("witness incident touched by sweep: %+v", gotW)
	}

	// Обратное включение ничего не воскрешает: оба инцидента остаются
	// закрытыми, новый (если условие всё ещё нарушено) откроет оценщик
	// следующим тиком.
	upd.Enabled = true
	if _, err := rules.Update(ctx, upd); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if got, _, _ := inc.GetByID(ctx, inT.ID); got.Status != "resolved" {
		t.Fatalf("re-enable must not resurrect incident: %+v", got)
	}
}

func TestIncidentServiceOpenClose(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rule, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	in, created, err := inc.Open(ctx, rule.ID, projectID, 150, false, "")
	if err != nil || !created {
		t.Fatalf("open = (%+v,%v,%v)", in, created, err)
	}
	// Повторный Open → created=false.
	if _, created2, _ := inc.Open(ctx, rule.ID, projectID, 160, false, ""); created2 {
		t.Fatalf("second open must be created=false")
	}
	// Bump.
	if err := inc.Bump(ctx, in.ID, 160, 160); err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Resolve, повторный → ok=false.
	if ok, _ := inc.Resolve(ctx, in.ID, 50); !ok {
		t.Fatalf("resolve must be ok=true")
	}
	if ok, _ := inc.Resolve(ctx, in.ID, 50); ok {
		t.Fatalf("second resolve must be ok=false")
	}
	// После закрытия новый Open создаёт (created=true).
	if _, created3, _ := inc.Open(ctx, rule.ID, projectID, 200, false, ""); !created3 {
		t.Fatalf("open after resolve must be created=true")
	}
	// List.
	if list, _ := inc.List(ctx, projectID, 10); len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
}

// TestIncidentServiceAcknowledge — B4: Acknowledge на открытом инциденте
// ставит acknowledged_at/acknowledged_by и возвращает ok=true; повторный
// вызов и вызов на закрытом инциденте — идемпотентно ok=false. scan (List)
// после ack отдаёт заполненные поля, до ack — nil.
func TestIncidentServiceAcknowledge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rule, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "metric-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	in, _, err := inc.Open(ctx, rule.ID, projectID, 150, false, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	list, err := inc.List(ctx, projectID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil || list[0].AcknowledgedBy != nil {
		t.Fatalf("до Acknowledge: list=%+v err=%v, want AcknowledgedAt/By nil", list, err)
	}

	ok, err := inc.Acknowledge(ctx, in.ID, projectID, userID)
	if err != nil || !ok {
		t.Fatalf("Acknowledge = (%v,%v), want (true,nil)", ok, err)
	}

	list, err = inc.List(ctx, projectID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt == nil {
		t.Fatalf("после Acknowledge: list=%+v err=%v, want AcknowledgedAt заполнено", list, err)
	}
	if list[0].AcknowledgedBy == nil || *list[0].AcknowledgedBy != userID {
		t.Fatalf("после Acknowledge: AcknowledgedBy = %v, want %d", list[0].AcknowledgedBy, userID)
	}

	// Повторный ack — идемпотентно ok=false.
	if ok2, err := inc.Acknowledge(ctx, in.ID, projectID, userID); err != nil || ok2 {
		t.Fatalf("повторный Acknowledge = (%v,%v), want (false,nil)", ok2, err)
	}

	// Acknowledge закрытого инцидента — ok=false.
	if _, err := inc.Resolve(ctx, in.ID, 50); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rule2, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "mem", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule2: %v", err)
	}
	closedIn, _, err := inc.Open(ctx, rule2.ID, projectID, 100, false, "")
	if err != nil {
		t.Fatalf("open closedIn: %v", err)
	}
	if _, err := inc.Resolve(ctx, closedIn.ID, 10); err != nil {
		t.Fatalf("resolve closedIn: %v", err)
	}
	if okClosed, err := inc.Acknowledge(ctx, closedIn.ID, projectID, userID); err != nil || okClosed {
		t.Fatalf("Acknowledge закрытого = (%v,%v), want (false,nil)", okClosed, err)
	}
}

// TestIncidentServiceAcknowledgeForeignProject — project_id — часть WHERE
// Acknowledge (defense-in-depth, зеркало uptime.DeleteWindow, B3).
func TestIncidentServiceAcknowledgeForeignProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	// Второй проект — руками, а не вторым seedProject(t, pool): seedProject
	// ключует email/org/project по t.Name(), одинаковому оба раза — второй
	// вызов упёрся бы в users_email_key. Тот же org_id вполне подходит: нужен
	// просто ДРУГОЙ project_id.
	var otherProjectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) SELECT org_id, $2, $2, 'go' FROM projects WHERE id = $1 RETURNING id",
		projectID, t.Name()+"-other").Scan(&otherProjectID); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "metric-ack-foreign@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	in, _, err := inc.Open(ctx, rule.ID, projectID, 150, false, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if ok, err := inc.Acknowledge(ctx, in.ID, otherProjectID, userID); err != nil || ok {
		t.Fatalf("Acknowledge с чужим project_id = (%v,%v), want (false,nil)", ok, err)
	}

	list, err := inc.List(ctx, projectID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil {
		t.Fatalf("после чужого Acknowledge: list=%+v err=%v, want AcknowledgedAt nil", list, err)
	}
}

func TestIncidentOpenConcurrentOnlyOneWins(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rule, _ := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})

	const n = 20
	var wg sync.WaitGroup
	created := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, c, err := inc.Open(ctx, rule.ID, projectID, 100+float64(i), false, "")
			if err != nil {
				t.Errorf("open %d: %v", i, err)
			}
			created[i] = c
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range created {
		if c {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent opens created=true count = %d, want exactly 1", wins)
	}
}
