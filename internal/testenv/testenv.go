package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"

	// Без него в slim-контейнере (нет /usr/share/zoneinfo) тесты зон падали не по вине продукта.
	_ "time/tzdata"
)

const (
	postgresImage   = "postgres:17-alpine"
	clickhouseImage = "clickhouse/clickhouse-server:25.3-alpine"
)

// Ryuk сносит контейнеры через 10с после отключения последнего процесса сессии — общие
// контейнеры переживают паузы между пакетами и прогонами, он снёс бы их посреди чужой работы.
func init() {
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// Версия образа в имени: бамп версии создаёт новый контейнер, а не переиспользует старый.
var (
	postgresReuseName   = reuseName("postgres", postgresImage)
	clickhouseReuseName = reuseName("clickhouse", clickhouseImage)
)

// По этому префиксу контейнеры находит `make test-env-down` — единственная явная уборка.
const reuseNamePrefix = "gotcha-test-"

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

// Клиент собран на адрес порта, на котором никто не слушает — connection refused
// приходит сразу, без ожидания dial-таймаута.
func BrokenCH(t *testing.T) driver.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve closed port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return openBrokenCH(t, addr)
}

// Сокет принимает соединение и тут же закрывает его (драйвер получает EOF на рукопожатии);
// счётчик — число принятых соединений.
func BrokenCHCounting(t *testing.T) (driver.Conn, *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var attempts atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return openBrokenCH(t, l.Addr().String()), &attempts
}

func openBrokenCH(t *testing.T, addr string) driver.Conn {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: "gotcha", Username: "gotcha", Password: "gotcha"},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("open broken clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
