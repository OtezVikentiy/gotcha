package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestConcurrentIndexOnePerExecContext — эмпирическая проверка допущения,
// на котором держится ДИСЦИПЛИНА «один CREATE/DROP INDEX CONCURRENTLY на файл
// миграции» во всём подпроекте «цена запросов» (находка №5, task-1-report.md,
// 2026-08-02). Не разовый эксперимент, а постоянный сторож: допущение
// определило число файлов во всех задачах подпроекта, и его тихая поломка
// (апгрейд golang-migrate, включение x-multi-statement, смена версии
// PostgreSQL) сделала бы миграции неприменимыми без единого изменения в
// самих файлах миграций — там менять будет нечего, сломается ИМЕННО это
// допущение.
//
// Факт: golang-migrate шлёт содержимое файла миграции ОДНИМ вызовом
// ExecContext на *sql.DB, открытом через github.com/jackc/pgx/v5/stdlib —
// ровно тот же драйвер и тот же метод воспроизведены здесь напрямую (см.
// database/pgx/v5/pgx.go: runStatement). PostgreSQL исполняет simple-query
// строку с НЕСКОЛЬКИМИ операторами как одну неявную транзакцию, а
// CREATE/DROP INDEX CONCURRENTLY внутри транзакционного блока запрещены
// (SQLSTATE 25001); один оператор транзакции не открывает. x-multi-statement
// в проекте не включён (см. MigrateCH в migrate.go), поэтому вывод общий для
// всех migrations/pg.
func TestConcurrentIndexOnePerExecContext(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	dsn := testenv.PostgresDSN(t)

	// Голый *sql.DB на драйвере "pgx" (jackc/pgx/v5/stdlib) — тот же самый
	// драйвер, что golang-migrate/database/pgx/v5 использует внутри себя. Не
	// через testenv.MigratedPG: проверке не нужна схема продукта, только сам
	// факт «сколько CONCURRENTLY проходит одним ExecContext».
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()
	ctx := context.Background()

	if _, err := sqlDB.ExecContext(ctx, "CREATE TABLE probe (id bigint PRIMARY KEY, v int, w int)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}

	// Один CREATE INDEX CONCURRENTLY — обязан проходить: это и есть форма,
	// в которой сейчас лежат все шесть миграций индексов чистильщика
	// (0031-0036). Если это упадёт, все шесть станут неприменимы.
	if _, err := sqlDB.ExecContext(ctx, "CREATE INDEX CONCURRENTLY probe_v_idx ON probe (v)"); err != nil {
		t.Fatalf("один CREATE INDEX CONCURRENTLY в файле перестал проходить (err=%v) — допущение, на котором держится 0031-0036 и весь подпроект, больше не верно", err)
	}

	// Два CREATE INDEX CONCURRENTLY в одном ExecContext — ровно то, что
	// получилось бы, положи кто-то оба индекса в один файл. Обязаны упасть
	// именно с "cannot run inside a transaction block": если однажды
	// перестанут падать, значит PostgreSQL/драйвер сменили поведение, и
	// дисциплину «один индекс на файл» можно (и стоит) пересмотреть — но
	// молча этого не заметить, а прочитать в красном тесте.
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
