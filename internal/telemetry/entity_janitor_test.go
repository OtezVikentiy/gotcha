package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

var entitySeq atomic.Int64

// uniformRetention — один и тот же срок для всех классов данных. Тесты ниже
// проверяют не разделение сроков (для этого есть
// TestEntityJanitorUsesPerEntityRetention), а сам механизм удаления, и им
// нужен ровно прежний, общий срок.
func uniformRetention(d time.Duration) telemetry.Retentions {
	return telemetry.Retentions{Events: d, Metrics: d, Profiles: d, Incidents: d}
}

// newEntityProject создаёт организацию с проектом. Каждый тест работает в своём
// проекте: контейнер PostgreSQL переиспользуется между запусками, и общие
// project_id связали бы тесты друг с другом.
func newEntityProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := entitySeq.Add(1)
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("janitor%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Janitor',1000000) RETURNING id",
		fmt.Sprintf("janitor-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id",
		orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// insertIssue добавляет группу с заданным возрастом последнего события.
func insertIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, fingerprint string, lastSeen time.Time, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO issues (project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen)
		 VALUES ($1,$2,'boom','app.go','error',$3,$4,$4,1) RETURNING id`,
		projectID, fingerprint, status, lastSeen).Scan(&id); err != nil {
		t.Fatalf("insert issue %s: %v", fingerprint, err)
	}
	return id
}

// insertMonitorIncident добавляет монитор с инцидентом. resolvedAt == nil —
// инцидент открыт.
func insertMonitorIncident(t *testing.T, pool *pgxpool.Pool, projectID int64, startedAt time.Time, resolvedAt *time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	n := entitySeq.Add(1)
	var monitorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, kind, interval_seconds, config)
		 VALUES ($1,$2,'http',60,'{"url":"https://example.com"}'::jsonb) RETURNING id`,
		projectID, fmt.Sprintf("mon-%d", n)).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO incidents (monitor_id, started_at, resolved_at, cause)
		 VALUES ($1,$2,$3,'down') RETURNING id`,
		monitorID, startedAt, resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	return id
}

func issueExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM issues WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("issue exists %d: %v", id, err)
	}
	return exists
}

func incidentExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM incidents WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("incident exists %d: %v", id, err)
	}
	return exists
}

// TestEntityJanitorPurgesExpiredKeepsFresh — основное правило: группа, событий
// которой уже нет в ClickHouse, не должна оставаться в списке проблем.
func TestEntityJanitorPurgesExpiredKeepsFresh(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	old := insertIssue(t, pool, pid, "old", now.Add(-40*24*time.Hour), "unresolved")
	fresh := insertIssue(t, pool, pid, "fresh", now.Add(-2*24*time.Hour), "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: uniformRetention(30 * 24 * time.Hour)}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if issueExists(t, pool, old) {
		t.Errorf("группа с последним событием 40 дней назад жива при сроке хранения 30 дней")
	}
	if !issueExists(t, pool, fresh) {
		t.Errorf("группа с последним событием 2 дня назад удалена при сроке хранения 30 дней")
	}
	if j.Purged() < 1 {
		t.Errorf("Purged() = %d, ожидалось хотя бы 1 удаление", j.Purged())
	}
}

// TestEntityJanitorKeepsOpenIncident — открытый инцидент описывает то, что
// происходит сейчас: его возраст не значит, что проблему можно перестать
// показывать.
func TestEntityJanitorKeepsOpenIncident(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	longAgo := now.Add(-90 * 24 * time.Hour)
	resolved := now.Add(-89 * 24 * time.Hour)

	open := insertMonitorIncident(t, pool, pid, longAgo, nil)
	closed := insertMonitorIncident(t, pool, pid, longAgo, &resolved)

	j := &telemetry.EntityJanitor{Pool: pool, Retention: uniformRetention(30 * 24 * time.Hour)}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !incidentExists(t, pool, open) {
		t.Errorf("открытый инцидент удалён по возрасту — активная проблема стала невидимой")
	}
	if incidentExists(t, pool, closed) {
		t.Errorf("закрытый 89 дней назад инцидент жив при сроке хранения 30 дней")
	}
}

// TestEntityJanitorPurgesBeyondOneBatch — чистка не должна останавливаться на
// первом батче: иначе при накопленном за месяцы объёме проход удалял бы
// entityBatchSize строк в час и никогда не догонял приём.
func TestEntityJanitorPurgesBeyondOneBatch(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	const total = 1500 // больше одного батча (1000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO issues (project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen)
		 SELECT $1, 'batch-' || g, 'boom', 'app.go', 'error', 'unresolved',
		        now() - interval '40 days', now() - interval '40 days', 1
		 FROM generate_series(1, $2) g`, pid, total); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	j := &telemetry.EntityJanitor{Pool: pool, Retention: uniformRetention(30 * 24 * time.Hour)}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var left int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM issues WHERE project_id = $1", pid).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("после прохода осталось %d из %d просроченных групп — чистка остановилась на первом батче", left, total)
	}
}

// TestEntityJanitorCascadesToChildRows — удаление группы должно убирать и
// связанные с ней строки, иначе окружения и записи глушения алертов остаются
// сиротами.
func TestEntityJanitorCascadesToChildRows(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	id := insertIssue(t, pool, pid, "with-children", time.Now().UTC().Add(-40*24*time.Hour), "unresolved")
	if _, err := pool.Exec(ctx,
		"INSERT INTO issue_environments (issue_id, project_id, environment) VALUES ($1,$2,'production')", id, pid); err != nil {
		t.Fatalf("insert issue_environments: %v", err)
	}

	j := &telemetry.EntityJanitor{Pool: pool, Retention: uniformRetention(30 * 24 * time.Hour)}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var children int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM issue_environments WHERE issue_id = $1", id).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if children != 0 {
		t.Errorf("после удаления группы осталось %d связанных окружений", children)
	}
}

// TestEntityJanitorNoRetentionKeepsEverything — без заданного срока хранения
// продукт ничего не удаляет: молча вычищать данные у того, кто хранение не
// настраивал, нельзя.
func TestEntityJanitorNoRetentionKeepsEverything(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	ancient := insertIssue(t, pool, pid, "ancient", time.Now().UTC().Add(-5*365*24*time.Hour), "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{}}
	n, err := j.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Errorf("Tick при Retention=0 удалил %d строк", n)
	}
	if !issueExists(t, pool, ancient) {
		t.Errorf("группа пятилетней давности удалена при незаданном сроке хранения")
	}
}

// insertProfileRegression добавляет ЗАКРЫТУЮ регрессию профиля с заданным
// моментом закрытия.
func insertProfileRegression(t *testing.T, pool *pgxpool.Pool, projectID int64, resolvedAt time.Time) int64 {
	t.Helper()
	n := entitySeq.Add(1)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO profile_regressions
		 (project_id, service, profile_type, function, status,
		  baseline_share, peak_share, current_share, started_at, resolved_at)
		 VALUES ($1, 'api', 'cpu', $2, 'resolved', 0.1, 0.5, 0.2, $3, $3) RETURNING id`,
		projectID, fmt.Sprintf("fn-%d", n), resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert profile regression: %v", err)
	}
	return id
}

// insertPerfRegression добавляет ЗАКРЫТУЮ регрессию производительности.
func insertPerfRegression(t *testing.T, pool *pgxpool.Pool, projectID int64, resolvedAt time.Time) int64 {
	t.Helper()
	n := entitySeq.Add(1)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO perf_regressions
		 (project_id, target_kind, target, metric, status,
		  baseline_value, peak_value, current_value, started_at, resolved_at)
		 VALUES ($1, 'endpoint_p95', $2, 'duration', 'resolved', 100, 500, 120, $3, $3) RETURNING id`,
		projectID, fmt.Sprintf("/api/%d", n), resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert perf regression: %v", err)
	}
	return id
}

// insertMetricIncident добавляет ЗАКРЫТЫЙ инцидент по метрике вместе с
// правилом, на которое он ссылается.
func insertMetricIncident(t *testing.T, pool *pgxpool.Pool, projectID int64, resolvedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	n := entitySeq.Add(1)
	var ruleID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold, window_seconds)
		 VALUES ($1, $2, 'avg', 'gt', 100, 300) RETURNING id`,
		projectID, fmt.Sprintf("http.server.duration.%d", n)).Scan(&ruleID); err != nil {
		t.Fatalf("insert metric rule: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO metric_incidents (rule_id, project_id, status, peak_value, current_value, started_at, resolved_at)
		 VALUES ($1, $2, 'resolved', 500, 120, $3, $3) RETURNING id`,
		ruleID, projectID, resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert metric incident: %v", err)
	}
	return id
}

// rowExists — жива ли строка таблицы. Имя таблицы приходит из литералов теста.
func rowExists(t *testing.T, pool *pgxpool.Pool, table string, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("%s exists %d: %v", table, id, err)
	}
	return exists
}

// TestEntityJanitorUsesPerEntityRetention — находка №108: все шесть правил жили
// одним GOTCHA_RETENTION_DAYS, хотя сроков в продукте четыре.
//
// Регрессия профиля переживала свои сэмплы на восемьдесят три дня — карточка
// открывалась, а флеймграфа за ней уже не было; инцидент метрики переживал
// точки метрик на шестьдесят. Проверяем на одном и том же возрасте закрытия,
// что каждое правило смотрит на СВОЙ срок: удаляется то, что пережило свою
// телеметрию, и остаётся то, что нет.
func TestEntityJanitorUsesPerEntityRetention(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	resolved := time.Now().UTC().Add(-10 * 24 * time.Hour)
	profileReg := insertProfileRegression(t, pool, pid, resolved)
	perfReg := insertPerfRegression(t, pool, pid, resolved)
	metricInc := insertMetricIncident(t, pool, pid, resolved)
	uptimeInc := insertMonitorIncident(t, pool, pid, resolved.Add(-time.Hour), &resolved)

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{
		Events:    90 * 24 * time.Hour,
		Metrics:   30 * 24 * time.Hour,
		Profiles:  7 * 24 * time.Hour,
		Incidents: 90 * 24 * time.Hour,
	}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if rowExists(t, pool, "profile_regressions", profileReg) {
		t.Errorf("регрессия профиля, закрытая 10 дней назад, жива при сроке профилей 7 дней — карточка без сэмплов")
	}
	if !rowExists(t, pool, "perf_regressions", perfReg) {
		t.Errorf("регрессия производительности, закрытая 10 дней назад, удалена при сроке событий 90 дней")
	}
	if !rowExists(t, pool, "metric_incidents", metricInc) {
		t.Errorf("инцидент метрики, закрытый 10 дней назад, удалён при сроке метрик 30 дней")
	}
	if !incidentExists(t, pool, uptimeInc) {
		t.Errorf("инцидент аптайма, закрытый 10 дней назад, удалён при своём сроке 90 дней")
	}
}

// insertHost добавляет хост проекта с заданным last_seen.
func insertHost(t *testing.T, pool *pgxpool.Pool, projectID int64, lastSeen time.Time) int64 {
	t.Helper()
	n := entitySeq.Add(1)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, first_seen, last_seen)
		 VALUES ($1, $2, $3, $3) RETURNING id`,
		projectID, fmt.Sprintf("host-%d", n), lastSeen).Scan(&id); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	return id
}

// insertHostIncident добавляет инцидент хоста (открытый при resolvedAt=nil).
func insertHostIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, resolvedAt *time.Time) int64 {
	t.Helper()
	status := "resolved"
	if resolvedAt == nil {
		status = "open"
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO host_incidents (project_id, host_id, kind, status, resolved_at)
		 VALUES ($1, $2, 'disk', $3, $4) RETURNING id`,
		projectID, hostID, status, resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert host incident: %v", err)
	}
	return id
}

// TestEntityJanitorPurgesStaleHostsCascadesIncidents — находка A1 (пороги по
// хостам): хост, не подававший признаков жизни дольше срока хранения метрик,
// в ClickHouse уже ничего не покажет — держать его строкой в списке хостов
// незачем. Правило смотрит на last_seen БЕЗ closedOnly (как issues), и
// host_incidents хоста уходят каскадом FK — в том числе открытые.
//
// Молчаливым исчезновением открытого инцидента это не становится: в проде на
// правило hosts повешен хук PreDelete (host.Retirer), который перед удалением
// закрывает инциденты и рассылает уведомление о снятии хоста с наблюдения (см.
// host.TestEntityJanitorRetiresHostsBeforeDelete). Здесь хука нет намеренно —
// проверяется само правило.
func TestEntityJanitorPurgesStaleHostsCascadesIncidents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	stale := insertHost(t, pool, pid, now.Add(-40*24*time.Hour))
	fresh := insertHost(t, pool, pid, now.Add(-2*24*time.Hour))

	staleOpen := insertHostIncident(t, pool, pid, stale, nil)
	// Закрыт два дня назад: по своему сроку инцидент живой, унести его мог
	// только каскад от хоста.
	freshResolvedAt := now.Add(-2 * 24 * time.Hour)
	staleClosed := insertHostIncident(t, pool, pid, stale, &freshResolvedAt)

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if rowExists(t, pool, "hosts", stale) {
		t.Errorf("хост, молчащий 40 дней, жив при сроке метрик 30 дней")
	}
	if !rowExists(t, pool, "hosts", fresh) {
		t.Errorf("хост, видевший событие 2 дня назад, удалён при сроке метрик 30 дней")
	}
	if rowExists(t, pool, "host_incidents", staleOpen) {
		t.Errorf("открытый инцидент удалённого хоста пережил каскад FK")
	}
	if rowExists(t, pool, "host_incidents", staleClosed) {
		t.Errorf("инцидент удалённого хоста пережил каскад FK (свой срок ещё не истёк — унести его мог только каскад)")
	}
}

// TestEntityJanitorPreDeleteHookSeesBatchBeforeDelete — контракт хука: он
// получает идентификаторы батча ДО удаления и видит строки ещё живыми. На этом
// стоит снятие хоста с наблюдения (host.Retirer): закрыть инциденты и
// разослать уведомления можно только пока хост существует.
func TestEntityJanitorPreDeleteHookSeesBatchBeforeDelete(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	stale := insertHost(t, pool, pid, now.Add(-40*24*time.Hour))
	fresh := insertHost(t, pool, pid, now.Add(-2*24*time.Hour))

	var got []int64
	aliveInHook := false
	j := &telemetry.EntityJanitor{
		Pool:      pool,
		Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour},
		PreDelete: map[string]telemetry.PreDeleteHook{
			"hosts": func(_ context.Context, ids []int64) error {
				got = append(got, ids...)
				aliveInHook = rowExists(t, pool, "hosts", stale)
				return nil
			},
		},
	}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(got) != 1 || got[0] != stale {
		t.Errorf("хук получил %v, want [%d] — только истёкший хост", got, stale)
	}
	if !aliveInHook {
		t.Errorf("к моменту вызова хука строка хоста уже удалена — закрывать инциденты будет некому")
	}
	if rowExists(t, pool, "hosts", stale) {
		t.Errorf("истёкший хост не удалён после успешного хука")
	}
	if !rowExists(t, pool, "hosts", fresh) {
		t.Errorf("живой хост удалён")
	}
}

// TestEntityJanitorPreDeleteHookErrorKeepsRows — провал хука отменяет удаление
// батча. Удалить строки, о которых не удалось сообщить, — ровно тот
// молчаливый исход, ради которого хук и заведён; строки дождутся следующего
// прохода.
func TestEntityJanitorPreDeleteHookErrorKeepsRows(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	stale := insertHost(t, pool, pid, now.Add(-40*24*time.Hour))
	// Соседнее правило (host_incidents) обязано отработать: отказ одного
	// правила не отменяет остальные.
	liveHost := insertHost(t, pool, pid, now.Add(-time.Hour))
	oldResolvedAt := now.Add(-40 * 24 * time.Hour)
	oldIncident := insertHostIncident(t, pool, pid, liveHost, &oldResolvedAt)

	j := &telemetry.EntityJanitor{
		Pool:      pool,
		Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour},
		PreDelete: map[string]telemetry.PreDeleteHook{
			"hosts": func(context.Context, []int64) error {
				return errors.New("канал недоступен")
			},
		},
	}
	// Проход в целом не проваливается: отказ таблицы логируется и не отменяет
	// остальные (см. Tick).
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !rowExists(t, pool, "hosts", stale) {
		t.Errorf("истёкший хост удалён, хотя хук провалился")
	}
	if rowExists(t, pool, "host_incidents", oldIncident) {
		t.Errorf("правило host_incidents не отработало из-за отказа соседнего правила")
	}
}

// TestEntityJanitorPreDeleteHookOnlyForItsTable — хук привязан к таблице
// своего правила: чужие правила про него не знают и работают прежним
// однооператорным путём.
func TestEntityJanitorPreDeleteHookOnlyForItsTable(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	staleIssue := insertIssue(t, pool, pid, "hook-foreign-table", now.Add(-40*24*time.Hour), "resolved")

	called := false
	j := &telemetry.EntityJanitor{
		Pool:      pool,
		Retention: telemetry.Retentions{Events: 30 * 24 * time.Hour},
		PreDelete: map[string]telemetry.PreDeleteHook{
			"hosts": func(context.Context, []int64) error {
				called = true
				return nil
			},
		},
	}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if called {
		t.Errorf("хук правила hosts вызван при удалении issues")
	}
	if rowExists(t, pool, "issues", staleIssue) {
		t.Errorf("issue, не видевшая событий 40 дней, жива при сроке событий 30 дней")
	}
}

// TestEntityJanitorPurgesResolvedHostIncidents — ревью I3: у host_incidents
// не было своего правила, и каскад от hosts спасал только тогда, когда хост
// удалён целиком. У ЖИВОГО сервера, регулярно пробивающего порог, закрытые
// инциденты копились бы вечно. Срок — метрик: карточка закрытого инцидента
// показывает период, за который точек в ClickHouse уже нет.
func TestEntityJanitorPurgesResolvedHostIncidents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	// Хост живой (last_seen свежий) — его самого правило hosts не трогает, и
	// каскад в этом тесте ничего не удаляет: проверяется именно своё правило.
	liveHost := insertHost(t, pool, pid, now.Add(-time.Hour))

	oldResolvedAt := now.Add(-40 * 24 * time.Hour)
	oldResolved := insertHostIncident(t, pool, pid, liveHost, &oldResolvedAt)
	freshResolvedAt := now.Add(-2 * 24 * time.Hour)
	freshResolved := insertHostIncident(t, pool, pid, liveHost, &freshResolvedAt)
	stillOpen := insertHostIncident(t, pool, pid, liveHost, nil)

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if rowExists(t, pool, "host_incidents", oldResolved) {
		t.Errorf("инцидент, закрытый 40 дней назад, жив при сроке метрик 30 дней")
	}
	if !rowExists(t, pool, "host_incidents", freshResolved) {
		t.Errorf("инцидент, закрытый 2 дня назад, удалён при сроке метрик 30 дней")
	}
	if !rowExists(t, pool, "host_incidents", stillOpen) {
		t.Errorf("ОТКРЫТЫЙ инцидент удалён — он описывает то, что с хостом происходит сейчас")
	}
	if !rowExists(t, pool, "hosts", liveHost) {
		t.Errorf("живой хост удалён — сработало не то правило")
	}
}

// TestEntityJanitorHostsUseMetricRetentionNotEvents — хосты живут сроком
// метрик (GOTCHA_METRIC_RETENTION_DAYS), а не сроком событий: инстанс с
// долгим хранением событий, но коротким — метрик, не обязан помнить хосты,
// метрики которых уже вычищены из ClickHouse.
func TestEntityJanitorHostsUseMetricRetentionNotEvents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	old := insertHost(t, pool, pid, time.Now().UTC().Add(-10*24*time.Hour))

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{
		Events:  90 * 24 * time.Hour, // событий срок большой — не должен спасти хост
		Metrics: 7 * 24 * time.Hour,
	}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if rowExists(t, pool, "hosts", old) {
		t.Errorf("хост, молчащий 10 дней, жив при сроке МЕТРИК 7 дней (правило смотрит не на срок событий)")
	}
}

// TestEntityJanitorHostsZeroMetricRetentionKeepsHosts — нулевой срок метрик
// выключает удаление ИМЕННО правила hosts, не задевая соседние правила с
// другим классом (по образцу TestEntityJanitorZeroClassKeepsItsEntities, но
// для конкретно добавленного правила hosts/retentionMetrics).
func TestEntityJanitorHostsZeroMetricRetentionKeepsHosts(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	old := insertHost(t, pool, pid, time.Now().UTC().Add(-400*24*time.Hour))
	issueID := insertIssue(t, pool, pid, "hosts-zero-metrics", time.Now().UTC().Add(-400*24*time.Hour), "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{
		Events:  30 * 24 * time.Hour,
		Metrics: 0, // удаление хостов выключено
	}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !rowExists(t, pool, "hosts", old) {
		t.Errorf("хост удалён при нулевом сроке метрик (Metrics=0 должен выключать именно правило hosts)")
	}
	if issueExists(t, pool, issueID) {
		t.Errorf("группа не удалена при заданном сроке событий — нулевой срок метрик погасил весь проход")
	}
}

// TestEntityJanitorZeroClassKeepsItsEntities — нулевой срок класса выключает
// удаление только в его правилах, не задевая остальные.
func TestEntityJanitorZeroClassKeepsItsEntities(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	old := time.Now().UTC().Add(-400 * 24 * time.Hour)
	profileReg := insertProfileRegression(t, pool, pid, old)
	issueID := insertIssue(t, pool, pid, "zero-class", old, "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: telemetry.Retentions{
		Events:   30 * 24 * time.Hour,
		Profiles: 0, // удаление регрессий профилей выключено
	}}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !rowExists(t, pool, "profile_regressions", profileReg) {
		t.Errorf("регрессия профиля удалена при нулевом сроке профилей")
	}
	if issueExists(t, pool, issueID) {
		t.Errorf("группа не удалена при заданном сроке событий — нулевой срок соседнего класса погасил весь проход")
	}
}
