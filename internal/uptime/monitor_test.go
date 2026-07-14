package uptime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

var projectSeq atomic.Int64

// newProject: прямые вставки — uptime-пакет не зависит от org. Каждый вызов
// заводит свой уникальный slug/email — тесты нередко создают несколько
// проектов, а users.email и organizations.slug уникальны глобально.
func newProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := projectSeq.Add(1)
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("u%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Up',1000000) RETURNING id",
		fmt.Sprintf("up-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// newProjectInOrg заводит проект в УЖЕ существующей организации (см. newOrgID):
// нужно там, где проба и монитор обязаны жить в одном тенанте.
func newProjectInOrg(t *testing.T, pool *pgxpool.Pool, orgID int64) int64 {
	t.Helper()
	n := projectSeq.Add(1)
	var projectID int64
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,'API') RETURNING id",
		orgID, fmt.Sprintf("api-%d", n)).Scan(&projectID); err != nil {
		t.Fatalf("project in org %d: %v", orgID, err)
	}
	return projectID
}

func newChannel(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO alert_channels (project_id, kind, target) VALUES ($1,'email','a@example.com') RETURNING id",
		projectID).Scan(&id); err != nil {
		t.Fatalf("channel: %v", err)
	}
	return id
}

func httpConfig(t *testing.T, cfg uptime.HTTPConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal http config: %v", err)
	}
	return raw
}

func tcpConfig(t *testing.T, cfg uptime.TCPConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal tcp config: %v", err)
	}
	return raw
}

func dnsConfig(t *testing.T, cfg uptime.DNSConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal dns config: %v", err)
	}
	return raw
}

func heartbeatConfig(t *testing.T, cfg uptime.HeartbeatConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	return raw
}

func baseHTTPMonitor(projectID int64) uptime.Monitor {
	return uptime.Monitor{
		ProjectID:          projectID,
		Name:               "API health",
		Kind:               uptime.KindHTTP,
		Enabled:            true,
		IntervalSeconds:    60,
		TimeoutSeconds:     10,
		FailThreshold:      3,
		RecoveryThreshold:  2,
		Consensus:          uptime.ConsensusMajority,
		RemindEveryMinutes: 0,
		SSLAlertDays:       14,
	}
}

func TestCreateHTTPMonitorWithRegionsAndChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	chID := newChannel(t, pool, pid)

	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})

	created, err := svc.Create(ctx, m, []string{"local", "eu"}, []int64{chID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("Create: expected non-zero id")
	}
	if created.HeartbeatToken != "" {
		t.Errorf("HeartbeatToken = %q, want empty for http monitor", created.HeartbeatToken)
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	sort.Strings(got.Regions)
	if len(got.Regions) != 2 || got.Regions[0] != "eu" || got.Regions[1] != "local" {
		t.Errorf("Regions = %v, want [eu local]", got.Regions)
	}
	if len(got.ChannelIDs) != 1 || got.ChannelIDs[0] != chID {
		t.Errorf("ChannelIDs = %v, want [%d]", got.ChannelIDs, chID)
	}
	if got.Name != "API health" || got.Kind != uptime.KindHTTP {
		t.Errorf("Get: unexpected monitor %+v", got)
	}
}

func TestCreateDefaultRegions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})

	created, err := svc.Create(ctx, m, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Regions) != 1 || got.Regions[0] != "local" {
		t.Errorf("Regions = %v, want [local]", got.Regions)
	}
}

func TestHeartbeatTokenUniqueAndLookup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := uptime.Monitor{
		ProjectID:         pid,
		Name:              "Heartbeat A",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     3,
		RecoveryThreshold: 2,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}
	c1, err := svc.Create(ctx, m, nil, nil)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if c1.HeartbeatToken == "" {
		t.Fatalf("Create 1: expected non-empty heartbeat token")
	}

	m.Name = "Heartbeat B"
	c2, err := svc.Create(ctx, m, nil, nil)
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if c2.HeartbeatToken == "" || c2.HeartbeatToken == c1.HeartbeatToken {
		t.Fatalf("Create 2: token = %q, want unique non-empty", c2.HeartbeatToken)
	}

	found, err := svc.ByHeartbeatToken(ctx, c1.HeartbeatToken)
	if err != nil {
		t.Fatalf("ByHeartbeatToken: %v", err)
	}
	if found.ID != c1.ID {
		t.Errorf("ByHeartbeatToken: got id %d, want %d", found.ID, c1.ID)
	}

	if _, err := svc.ByHeartbeatToken(ctx, "does-not-exist"); !errors.Is(err, uptime.ErrNotFound) {
		t.Errorf("ByHeartbeatToken unknown: err = %v, want ErrNotFound", err)
	}
}

func TestListByProjectOrderedByName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	other := newProject(t, pool)

	mZ := baseHTTPMonitor(pid)
	mZ.Name = "Zebra"
	mZ.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/z"})
	if _, err := svc.Create(ctx, mZ, []string{"local"}, nil); err != nil {
		t.Fatalf("create zebra: %v", err)
	}

	mA := baseHTTPMonitor(pid)
	mA.Name = "Alpha"
	mA.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/a"})
	if _, err := svc.Create(ctx, mA, []string{"local"}, nil); err != nil {
		t.Fatalf("create alpha: %v", err)
	}

	mOther := baseHTTPMonitor(other)
	mOther.Name = "Other project"
	mOther.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/o"})
	if _, err := svc.Create(ctx, mOther, []string{"local"}, nil); err != nil {
		t.Fatalf("create other: %v", err)
	}

	list, err := svc.List(ctx, pid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "Zebra" {
		t.Fatalf("List = %+v, want [Alpha Zebra]", list)
	}
	if len(list[0].Regions) != 1 || list[0].Regions[0] != "local" {
		t.Errorf("List: regions not populated: %+v", list[0])
	}
}

func TestUpdateReplacesFieldsRegionsAndChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch1 := newChannel(t, pool, pid)
	ch2 := newChannel(t, pool, pid)

	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, []string{"local"}, []int64{ch1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated := created
	updated.Name = "API health v2"
	updated.IntervalSeconds = 120
	updated.TimeoutSeconds = 20
	updated.Config = httpConfig(t, uptime.HTTPConfig{Method: "POST", URL: "https://example.com/v2"})
	if err := svc.Update(ctx, updated, []string{"eu", "us"}, []int64{ch2}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Name != "API health v2" || got.IntervalSeconds != 120 || got.TimeoutSeconds != 20 {
		t.Errorf("Update: unexpected monitor %+v", got)
	}
	sort.Strings(got.Regions)
	if len(got.Regions) != 2 || got.Regions[0] != "eu" || got.Regions[1] != "us" {
		t.Errorf("Update: regions = %v, want [eu us]", got.Regions)
	}
	if len(got.ChannelIDs) != 1 || got.ChannelIDs[0] != ch2 {
		t.Errorf("Update: channels = %v, want [%d]", got.ChannelIDs, ch2)
	}
	if got.Kind != uptime.KindHTTP {
		t.Errorf("Update: kind changed to %v", got.Kind)
	}
}

func TestUpdateDoesNotChangeKindOrHeartbeatToken(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := uptime.Monitor{
		ProjectID:         pid,
		Name:              "Heartbeat",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     3,
		RecoveryThreshold: 2,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}
	created, err := svc.Create(ctx, m, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated := created
	updated.Kind = uptime.KindTCP // должно быть проигнорировано
	updated.HeartbeatToken = "attacker-supplied-token"
	updated.Config = heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: 90})
	if err := svc.Update(ctx, updated, nil, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != uptime.KindHeartbeat {
		t.Errorf("Kind = %v, want heartbeat (unchanged)", got.Kind)
	}
	if got.HeartbeatToken != created.HeartbeatToken {
		t.Errorf("HeartbeatToken changed: got %q, want %q", got.HeartbeatToken, created.HeartbeatToken)
	}
}

func TestCreateChannelFromOtherProjectFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	other := newProject(t, pool)
	foreignChannel := newChannel(t, pool, other)

	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})

	if _, err := svc.Create(ctx, m, nil, []int64{foreignChannel}); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("Create with foreign channel: err = %v, want ErrInvalidMonitor", err)
	}
}

func TestCreateInvalidMonitors(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	cases := []struct {
		name string
		m    uptime.Monitor
	}{
		{
			name: "interval too small",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.IntervalSeconds = 10
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com"})
				return m
			}(),
		},
		{
			name: "timeout not less than interval",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.IntervalSeconds = 30
				m.TimeoutSeconds = 30
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com"})
				return m
			}(),
		},
		{
			name: "invalid url",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "not-a-url"})
				return m
			}(),
		},
		{
			name: "ftp scheme",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "ftp://example.com/file"})
				return m
			}(),
		},
		{
			name: "bad method",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "DELETE", URL: "https://example.com"})
				return m
			}(),
		},
		{
			name: "invalid consensus",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Consensus = "quorum"
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com"})
				return m
			}(),
		},
		{
			name: "bad tcp port",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Kind = uptime.KindTCP
				m.Config = tcpConfig(t, uptime.TCPConfig{Host: "db.internal", Port: 70000})
				return m
			}(),
		},
		{
			name: "tcp config for http kind",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Config = tcpConfig(t, uptime.TCPConfig{Host: "db.internal", Port: 5432})
				return m
			}(),
		},
		{
			name: "invalid dns record type",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Kind = uptime.KindDNS
				m.Config = dnsConfig(t, uptime.DNSConfig{Hostname: "example.com", RecordType: "PTR"})
				return m
			}(),
		},
		{
			name: "heartbeat grace too low",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Kind = uptime.KindHeartbeat
				m.Config = heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: 10})
				return m
			}(),
		},
		{
			name: "name exceeds 200 characters (201 Cyrillic)",
			m: func() uptime.Monitor {
				m := baseHTTPMonitor(pid)
				m.Name = strings.Repeat("я", 201)
				m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com"})
				return m
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(ctx, tc.m, nil, nil); !errors.Is(err, uptime.ErrInvalidMonitor) {
				t.Fatalf("Create: err = %v, want ErrInvalidMonitor", err)
			}
		})
	}
}

func TestDeleteCascadesRegionsAndChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch := newChannel(t, pool, pid)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, []string{"local"}, []int64{ch})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := svc.Get(ctx, created.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, created.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("Delete again: err = %v, want ErrNotFound", err)
	}

	var regionCount, channelCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM monitor_regions WHERE monitor_id=$1", created.ID).Scan(&regionCount); err != nil {
		t.Fatalf("count regions: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM monitor_channels WHERE monitor_id=$1", created.ID).Scan(&channelCount); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if regionCount != 0 || channelCount != 0 {
		t.Errorf("Delete: regions=%d channels=%d, want 0/0", regionCount, channelCount)
	}
}

func TestSetEnabled(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.Enabled {
		t.Fatalf("Create: expected Enabled=true")
	}

	if err := svc.SetEnabled(ctx, created.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled {
		t.Errorf("Enabled = true, want false after SetEnabled(false)")
	}

	if err := svc.SetEnabled(ctx, 999999999, true); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("SetEnabled unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestUnicodeNameAndRegionLimits(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)

	// Test: 200 Cyrillic characters in name should be accepted
	m := baseHTTPMonitor(pid)
	m.Name = strings.Repeat("я", 200)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create with 200 Cyrillic chars in name: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("Create with 200 Cyrillic chars: expected non-zero id")
	}

	// Test: 201 Cyrillic characters in name should be rejected
	m2 := baseHTTPMonitor(pid)
	m2.Name = strings.Repeat("я", 201)
	m2.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	if _, err := svc.Create(ctx, m2, nil, nil); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("Create with 201 Cyrillic chars in name: err = %v, want ErrInvalidMonitor", err)
	}

	// Test: 40 Cyrillic characters in region should be accepted
	m3 := baseHTTPMonitor(pid)
	m3.Name = "Test 40 chars region"
	m3.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created3, err := svc.Create(ctx, m3, []string{strings.Repeat("я", 40)}, nil)
	if err != nil {
		t.Fatalf("Create with 40 Cyrillic chars in region: %v", err)
	}
	if created3.ID == 0 {
		t.Fatalf("Create with 40 Cyrillic chars region: expected non-zero id")
	}

	// Test: 41 Cyrillic characters in region should be rejected
	m4 := baseHTTPMonitor(pid)
	m4.Name = "Test 41 chars region"
	m4.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	if _, err := svc.Create(ctx, m4, []string{strings.Repeat("я", 41)}, nil); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("Create with 41 Cyrillic chars in region: err = %v, want ErrInvalidMonitor", err)
	}
}
