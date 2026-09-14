package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// increase — новая агрегация правил (честный прирост monotonic-счётчика за окно,
// не средняя скорость); CHECK на aggregation обязан принять её, не потеряв старые правила.
func TestMigrate0096MetricRuleIncreaseAggregation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 95); err != nil {
		t.Fatalf("migrate to 95: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, ruleID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m96','M96',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m96','M96') RETURNING id", orgID)
	mustScan(t, pool, &ruleID,
		`INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		 VALUES ($1, 'req.total', 'sum', 'gt', 0) RETURNING id`, projectID)

	if _, err := pool.Exec(ctx,
		`INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		 VALUES ($1, 'deadlocks.total', 'increase', 'gt', 0)`, projectID); err == nil {
		t.Fatal("increase до миграции 96 должен быть отвергнут CHECK, а не принят")
	}

	if err := db.MigratePGTo(dsn, 96); err != nil {
		t.Fatalf("migrate to 96: %v", err)
	}

	var ruleCount int64
	mustScan(t, pool, &ruleCount, "SELECT count(*) FROM metric_alert_rules WHERE id = $1", ruleID)
	if ruleCount != 1 {
		t.Fatalf("старое правило должно пережить миграцию 96, count=%d, want 1", ruleCount)
	}

	var increaseID int64
	mustScan(t, pool, &increaseID,
		`INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		 VALUES ($1, 'deadlocks.total', 'increase', 'gt', 0) RETURNING id`, projectID)

	// Ловушка: живая increase-строка не должна ронять откат — down обязан сам
	// перевести её на sum перед тем, как вернуть строгий CHECK.
	if err := db.MigratePGTo(dsn, 95); err != nil {
		t.Fatalf("migrate down to 95 с живой increase-строкой: %v", err)
	}

	mustScan(t, pool, &ruleCount, "SELECT count(*) FROM metric_alert_rules WHERE id = $1", ruleID)
	if ruleCount != 1 {
		t.Fatalf("старое правило должно пережить откат до 95, count=%d, want 1", ruleCount)
	}

	var increaseAgg string
	if err := pool.QueryRow(ctx, "SELECT aggregation FROM metric_alert_rules WHERE id = $1", increaseID).
		Scan(&increaseAgg); err != nil {
		t.Fatalf("scan aggregation after rollback: %v", err)
	}
	if increaseAgg != "sum" {
		t.Fatalf("increase-правило после отката = %q, want конвертацию в sum, не потерю строки", increaseAgg)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		 VALUES ($1, 'deadlocks.total', 'increase', 'gt', 0)`, projectID); err == nil {
		t.Fatal("increase после отката до 95 должен быть отвергнут CHECK, а не принят")
	}
}
