package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// golang-migrate исполняет файл миграции ОДНИМ ExecContext; несколько операторов Postgres выполняет
// как одну неявную транзакцию, где CREATE/DROP INDEX CONCURRENTLY запрещён (SQLSTATE 25001).
func TestConcurrentIndexOnePerExecContext(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	dsn := testenv.PostgresDSN(t)

	// Не через testenv.MigratedPG — проверке нужен только сам факт, без схемы продукта.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()
	ctx := context.Background()

	if _, err := sqlDB.ExecContext(ctx, "CREATE TABLE probe (id bigint PRIMARY KEY, v int, w int)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}

	// Обязан проходить — в этой форме лежат миграции 0031-0036, иначе все шесть станут неприменимы.
	if _, err := sqlDB.ExecContext(ctx, "CREATE INDEX CONCURRENTLY probe_v_idx ON probe (v)"); err != nil {
		t.Fatalf("один CREATE INDEX CONCURRENTLY в файле перестал проходить (err=%v) — допущение, на котором держится 0031-0036 и весь подпроект, больше не верно", err)
	}

	// Два CONCURRENTLY в одном ExecContext должны упасть именно на transaction block — если однажды
	// перестанут, значит поведение Postgres/драйвера сменилось, и дисциплину стоит пересмотреть.
	_, err = sqlDB.ExecContext(ctx,
		"CREATE INDEX CONCURRENTLY probe_id_idx ON probe (id); CREATE INDEX CONCURRENTLY probe_w_idx ON probe (w)")
	if err == nil {
		t.Fatal("два CREATE INDEX CONCURRENTLY в одном ExecContext прошли без ошибки — допущение " +
			"«один индекс на файл» (task-1-report.md) больше не подтверждено эмпирически, дисциплину миграций подпроекта надо пересмотреть")
	}
	if !strings.Contains(err.Error(), "CONCURRENTLY") || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("два CREATE INDEX CONCURRENTLY упали, но НЕ по ожидаемой причине (не транзакционный блок): %v — дисциплину «один индекс на файл» стоит пересмотреть на основании этой, другой причины", err)
	}
}
