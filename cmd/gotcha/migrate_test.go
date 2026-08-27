package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrationStagesAreLogged: между «применяю миграции» и «слушаю» не было
// ни одной записи, хотя внутри — миграции двух СУБД и перезапись кусков по
// семи таблицам. Дежурный не мог отличить работу от зависания на блокировке,
// а перезапуск в этот момент даёт грязную схему.
//
// Гоняет РЕАЛЬНЫЕ миграции против свежих (немигрированных) контейнеров
// testenv, а не заглушку: иначе тест проверял бы, что migrationStage()
// вызывается, а не что за ним стоит настоящая, потенциально долгая работа.
func TestMigrationStagesAreLogged(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres/clickhouse containers")
	}
	pgDSN := testenv.PostgresDSN(t)
	chDSN := testenv.ClickHouseDSN(t)
	ctx := context.Background()

	pg, err := db.NewPostgres(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	defer pg.Close()
	ch, err := db.NewClickHouse(ctx, chDSN)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	defer ch.Close()

	cfg := Config{
		PostgresDSN:          pgDSN,
		ClickHouseDSN:        chDSN,
		AutoMigrate:          true,
		RetentionDays:        30,
		SpanRetentionDays:    30,
		MetricRetentionDays:  30,
		ProfileRetentionDays: 30,
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := applyMigrations(ctx, cfg, pg, ch); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	out := buf.String()

	// Ожидание блокировки — отдельная строка: снаружи (по логу) это
	// единственное, что неотличимо от зависания. Мало того, что обе строки
	// есть — «жду» обязана идти РАНЬШЕ «получил»: ради этого порядка этап и
	// заводили (дежурный должен увидеть именно факт ожидания, а не только
	// то, что оно когда-то случилось). Совпадение по одному только Contains
	// не отличило бы верный порядок от строк, случайно переставленных
	// местами при будущей правке — сравниваем позиции вхождений.
	waitIdx := strings.Index(out, "waiting for migration lock")
	acquiredIdx := strings.Index(out, "migration lock acquired")
	if waitIdx == -1 {
		t.Error("нет строки об ожидании блокировки миграций")
	}
	if acquiredIdx == -1 {
		t.Error("нет строки о получении блокировки — конец ожидания не зафиксирован")
	}
	if waitIdx != -1 && acquiredIdx != -1 && waitIdx > acquiredIdx {
		t.Errorf("«получил блокировку» встречается раньше «жду блокировку» в логе — дежурный не увидит, что процесс ждал:\n%s", out)
	}

	// Каждый этап — начало и конец с длительностью полем (не текстом: её
	// читают машины). Признаки совместимости схемы записываются ДВУМЯ
	// отдельными этапами, каждый сразу за миграцией своей схемы (W3-D,
	// запись 5) — а не одним общим хвостом после обеих, как раньше: иначе
	// сорванная CH-миграция при успешной PG оставляла PG без единой строки в
	// schema_compat и без возможности отката.
	for _, stage := range []string{
		"postgres schema migration",
		"postgres schema compatibility markers",
		"clickhouse schema migration",
		"clickhouse schema compatibility markers",
		"retention rollout",
	} {
		startLine := fmt.Sprintf(`msg="migration stage starting" stage=%q`, stage)
		if !strings.Contains(out, startLine) {
			t.Errorf("нет начала этапа %q в логе:\n%s", stage, out)
		}
		finishLine := fmt.Sprintf(`msg="migration stage finished" stage=%q`, stage)
		idx := strings.Index(out, finishLine)
		if idx == -1 {
			t.Errorf("нет конца этапа %q в логе:\n%s", stage, out)
			continue
		}
		end := idx + len(finishLine) + 40
		if end > len(out) {
			end = len(out)
		}
		if !strings.Contains(out[idx:end], "duration=") {
			t.Errorf("конец этапа %q без длительности (поле duration): %s", stage, out[idx:end])
		}
	}
}

// TestFailedCHMigrationLeavesPGRollbackable — сорванная CH-миграция при
// успешной PG не должна оставлять PG без маркеров совместимости (W3-D,
// запись 5): раньше recordSchemaCompat писал ОБЕ схемы одним хвостом ПОСЛЕ
// обеих миграций, и провал на ClickHouse означал, что PG-версии, УЖЕ реально
// применённые к базе, не получали ни строки в schema_compat — откат бинаря
// назад становился невозможен без восстановления из бэкапа. Теперь PG пишет
// свои маркеры сразу за своей миграцией, до попытки мигрировать CH.
func TestFailedCHMigrationLeavesPGRollbackable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres/clickhouse containers")
	}
	pgDSN := testenv.PostgresDSN(t)
	ctx := context.Background()

	pg, err := db.NewPostgres(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	defer pg.Close()
	// ch передаётся для сигнатуры applyMigrations, но реальной миграции CH не
	// требуется: она обязана провалиться раньше, на MigrateCH(cfg.ClickHouseDSN).
	ch, err := db.NewClickHouse(ctx, testenv.ClickHouseDSN(t))
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	defer ch.Close()

	cfg := Config{
		PostgresDSN:   pgDSN,
		ClickHouseDSN: "clickhouse://127.0.0.1:1/gotcha", // порт, где никто не слушает — быстрый отказ соединения
		AutoMigrate:   true,
	}

	if err := applyMigrations(ctx, cfg, pg, ch); err == nil {
		t.Fatal("applyMigrations с недоступным ClickHouse должен был вернуть ошибку")
	}

	var pgRows int
	if err := pg.QueryRow(ctx, "SELECT count(*) FROM schema_compat WHERE target = 'pg'").Scan(&pgRows); err != nil {
		t.Fatalf("count pg schema_compat rows: %v", err)
	}
	if pgRows == 0 {
		t.Error("после сорванной CH-миграции у PG нет ни одной строки в schema_compat — откат бинаря невозможен")
	}

	var chRows int
	if err := pg.QueryRow(ctx, "SELECT count(*) FROM schema_compat WHERE target = 'ch'").Scan(&chRows); err != nil {
		t.Fatalf("count ch schema_compat rows: %v", err)
	}
	if chRows != 0 {
		t.Errorf("CH-миграция провалилась, но schema_compat содержит %d строк для 'ch' — маркер записан раньше времени", chRows)
	}
}
