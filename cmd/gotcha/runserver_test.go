package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

func testServerConfig(t *testing.T, mode, addr, pgDSN, chDSN string) Config {
	t.Helper()
	getenv := getenvFrom(map[string]string{
		"GOTCHA_PG_DSN":      pgDSN,
		"GOTCHA_CH_DSN":      chDSN,
		"GOTCHA_LISTEN_ADDR": addr,
		"GOTCHA_BASE_URL":    "http://" + addr,
	})
	cfg, err := loadConfig(getenv, []string{"--mode=" + mode})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

func waitForHealthz(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not answer /healthz in time", addr)
}

func stopRunServer(t *testing.T, cancel context.CancelFunc, errCh chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServer did not return within 15s of ctx cancellation")
	}
}

func routeRegistered(t *testing.T, addr, method, path string) bool {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}

// Опечатка в гейте по cfg.Mode открыла бы или закрыла приём на неверной реплике,
// и без этого теста ни один другой не покраснел бы.
func TestRunServerModeGatesIngestAndWebSurfaces(t *testing.T) {
	pgDSN := testenv.PostgresDSN(t)
	chDSN := testenv.ClickHouseDSN(t)
	if err := db.MigratePG(pgDSN); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	if err := db.MigrateCH(chDSN); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}

	cases := []struct {
		mode           string
		wantIngestSeen bool
		wantWebSeen    bool
	}{
		{"web", false, true},
		{"ingest", true, false},
		{"all", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			addr := freeAddr(t)
			cfg := testServerConfig(t, tc.mode, addr, pgDSN, chDSN)

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- runServer(ctx, cfg, 0) }()
			t.Cleanup(func() { stopRunServer(t, cancel, errCh) })

			waitForHealthz(t, addr)

			if got := routeRegistered(t, addr, "POST", "/v1/logs"); got != tc.wantIngestSeen {
				t.Errorf("mode=%s: ingest route /v1/logs registered=%v, want %v", tc.mode, got, tc.wantIngestSeen)
			}
			if got := routeRegistered(t, addr, "GET", "/login"); got != tc.wantWebSeen {
				t.Errorf("mode=%s: web route /login registered=%v, want %v", tc.mode, got, tc.wantWebSeen)
			}
		})
	}
}

const runServerTestEventJSON = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error",` +
	`"exception":{"values":[{"type":"ValueError","value":"drain test",` +
	`"stacktrace":{"frames":[{"function":"do","module":"app.main","in_app":true}]}}]}}`

// Контекст отменяется до первого тика батчера, поэтому событие может доехать
// только принудительным закрытием в drain, а не штатным таймером.
func TestRunServerDrainFlushesBufferedEventBeforeExit(t *testing.T) {
	pgDSN := testenv.PostgresDSN(t)
	chDSN := testenv.ClickHouseDSN(t)
	if err := db.MigratePG(pgDSN); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	if err := db.MigrateCH(chDSN); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}

	ctx0 := context.Background()
	pool, err := db.NewPostgres(ctx0, pgDSN)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	defer pool.Close()
	ch, err := db.NewClickHouse(ctx0, chDSN)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	defer ch.Close()

	_, pid := newBootstrapOrgAndProject(t, pool, "runserver-drain")
	keys, err := org.NewService(pool, 1_000_000).CreateKeys(ctx0, pid, org.KindLegacy)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	key := keys[0].PublicKey

	addr := freeAddr(t)
	cfg := testServerConfig(t, "ingest", addr, pgDSN, chDSN)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runServer(ctx, cfg, 0) }()
	waitForHealthz(t, addr)

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s/api/%d/store/", addr, pid),
		strings.NewReader(runServerTestEventJSON))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post event status = %d, want 200", resp.StatusCode)
	}

	stopRunServer(t, cancel, errCh)

	var n uint64
	if err := ch.QueryRow(context.Background(),
		"SELECT count() FROM events WHERE project_id = ?", pid).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("events for project %d after graceful shutdown = %d, want 1 — drain did not flush the batcher buffer", pid, n)
	}
}
