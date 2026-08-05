// Package testenv поднимает PostgreSQL и ClickHouse в docker для
// интеграционных тестов. Все функции скипают тест при -short.
//
// Контейнеры переиспользуются (reuse by name): одноразовые контейнеры на
// каждый тест создавали шторм veth-событий, от которого переподключались Chrome
// и VK Workspace. Изоляция тестов обеспечивается уникальной базой на тест
// внутри общего контейнера.
//
// Живут они дольше прогона и убираются явно — `make test-env-down`; почему не
// автоматическим сборщиком, сказано у init ниже.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"

	// Часовые пояса вкомпилированы в тесты: зоны читают семь тестовых файлов, а
	// time/tzdata до сих пор подключался только в cmd/gotcha. В slim-контейнере
	// без /usr/share/zoneinfo эти тесты падали не по вине продукта.
	_ "time/tzdata"
)

const (
	postgresImage   = "postgres:17-alpine"
	clickhouseImage = "clickhouse/clickhouse-server:25.3-alpine"
)

// Сборщик контейнеров (Ryuk) для этих контейнеров отключён намеренно.
//
// Он владеет контейнерами ПОСЕССИОННО: сессия — это один вызов `go test`, и
// через 10 секунд после того, как от неё отключился последний процесс, сборщик
// сносит всё, что она создала. Наши контейнеры устроены иначе: они общие и
// переиспользуются по имени — и всеми пакетами внутри прогона, и следующими
// прогонами. Обе границы бьются с моделью владения:
//
//   - внутри прогона. `go test ./...` запускает по бинарнику на пакет, и в
//     паузе между двумя бинарниками к сборщику не подключён никто. С `-p 1`
//     (а он обязателен: параллельный старт контейнеров кладёт машину) пауза —
//     это компиляция и линковка следующего бинарника, под `-race` десятки
//     секунд. Сборщик успевает снести PostgreSQL и ClickHouse посреди прогона,
//     и следующий пакет получает «container is marked for removal and cannot
//     be started» или «connection refused» на порту исчезнувшего контейнера.
//     Так упали два прогона CI 2026-08-05: internal/profile на релизе v0.4.9 и
//     cmd/gotcha на замере покрытия v0.4.5 — оба без единой правки в
//     затронутых пакетах;
//   - между прогонами. Метка сессии остаётся на контейнере навсегда, а сменить
//     её у существующего контейнера docker не даёт. Значит, контейнер, который
//     переиспользовал следующий прогон, продолжает принадлежать сборщику
//     ПРЕДЫДУЩЕГО — и тот снесёт его посреди чужой работы.
//
// Любая выдержка — это ставка на то, что граница сессии не попадёт внутрь
// чужого использования. Ставку мы не делаем: контейнеры живут, пока их не
// снесут явно (`make test-env-down`), а имя с версией образа не даёт
// переиспользовать контейнер после бампа. Вернуть сборщик можно, выставив
// TESTCONTAINERS_RYUK_DISABLED=false в окружении.
func init() {
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// Имена переиспользуемых контейнеров несут версию образа.
//
// Раньше имя было постоянным («gotcha-test-postgres»), и после бампа версии
// образа тесты молча переиспользовали СТАРЫЙ контейнер: весь набор проверял не
// тот движок, который поедет в прод, и узнать об этом было неоткуда. Версия в
// имени делает контейнер другим объектом — старый остаётся, новый создаётся.
var (
	postgresReuseName   = reuseName("postgres", postgresImage)
	clickhouseReuseName = reuseName("clickhouse", clickhouseImage)
)

// reuseNamePrefix — общий префикс имён тестовых контейнеров. По нему их
// находит `make test-env-down`: единственная явная уборка, раз сборщик
// отключён (см. init).
const reuseNamePrefix = "gotcha-test-"

// reuseName собирает имя вида gotcha-test-postgres-17-alpine.
func reuseName(role, image string) string {
	tag := image
	if i := strings.LastIndex(image, ":"); i >= 0 {
		tag = image[i+1:]
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, tag)
	return reuseNamePrefix + role + "-" + safe
}

var (
	pgOnce sync.Once
	pgPool *pgxpool.Pool // админ-пул к базе gotcha общего контейнера
	pgDSN  string
	pgErr  error

	chOnce sync.Once
	chConn driver.Conn // админ-соединение с базой gotcha общего контейнера
	chDSN  string
	chErr  error
)

// PostgresDSN возвращает DSN уникальной базы в общем контейнере PostgreSQL.
// База удаляется в t.Cleanup, контейнер остаётся жить.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	ctx := context.Background()

	pgOnce.Do(func() {
		ctr, err := tcpostgres.Run(ctx, postgresImage,
			tcpostgres.WithDatabase("gotcha"),
			tcpostgres.WithUsername("gotcha"),
			tcpostgres.WithPassword("gotcha"),
			tcpostgres.BasicWaitStrategies(),
			testcontainers.WithReuseByName(postgresReuseName),
		)
		if err != nil {
			pgErr = fmt.Errorf("start postgres container: %w", err)
			return
		}
		pgDSN, pgErr = ctr.ConnectionString(ctx, "sslmode=disable")
		if pgErr != nil {
			return
		}
		pgPool, pgErr = db.NewPostgres(ctx, pgDSN)
	})
	if pgErr != nil {
		t.Fatalf("shared postgres: %v", pgErr)
	}

	name := "t_" + randHex(8)
	// Параллельные CREATE DATABASE конкурируют за template1 — ретраим.
	var err error
	for i := 0; i < 5; i++ {
		_, err = pgPool.Exec(ctx, "CREATE DATABASE "+name)
		if err == nil || !strings.Contains(err.Error(), "is being accessed by other users") {
			break
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pgPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	return swapDatabase(t, pgDSN, name)
}

// ClickHouseDSN возвращает DSN уникальной базы в общем контейнере ClickHouse.
// База удаляется в t.Cleanup, контейнер остаётся жить.
func ClickHouseDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	ctx := context.Background()

	chOnce.Do(func() {
		ctr, err := tcclickhouse.Run(ctx, clickhouseImage,
			tcclickhouse.WithDatabase("gotcha"),
			tcclickhouse.WithUsername("gotcha"),
			tcclickhouse.WithPassword("gotcha"),
			testcontainers.WithReuseByName(clickhouseReuseName),
		)
		if err != nil {
			chErr = fmt.Errorf("start clickhouse container: %w", err)
			return
		}
		chDSN, chErr = ctr.ConnectionString(ctx)
		if chErr != nil {
			return
		}
		connCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		chConn, chErr = db.NewClickHouse(connCtx, chDSN)
	})
	if chErr != nil {
		t.Fatalf("shared clickhouse: %v", chErr)
	}

	name := "t_" + randHex(8)
	if err := chConn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = chConn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})

	return swapDatabase(t, chDSN, name)
}

// MigratedPG выдаёт уникальную базу PostgreSQL, применяет все миграции
// и возвращает пул.
func MigratedPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// MigratedCH выдаёт уникальную базу ClickHouse, применяет миграции
// и возвращает соединение.
func MigratedCH(t *testing.T) driver.Conn {
	t.Helper()
	dsn := ClickHouseDSN(t)
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// swapDatabase заменяет имя базы (path) в URL-образном DSN.
func swapDatabase(t *testing.T, dsn, dbName string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn %q: %v", dsn, err)
	}
	u.Path = "/" + dbName
	return u.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
