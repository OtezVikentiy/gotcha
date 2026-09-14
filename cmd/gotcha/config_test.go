package main

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "all" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "all")
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.PostgresDSN != "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable" {
		t.Errorf("PostgresDSN = %q", cfg.PostgresDSN)
	}
	if cfg.ClickHouseDSN != "clickhouse://localhost:9000/gotcha" {
		t.Errorf("ClickHouseDSN = %q", cfg.ClickHouseDSN)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90", cfg.RetentionDays)
	}
	if cfg.SpanRetentionDays != 30 {
		t.Errorf("SpanRetentionDays = %d, want 30", cfg.SpanRetentionDays)
	}
	if cfg.LogRetentionDays != 14 {
		t.Errorf("LogRetentionDays = %d, want 14", cfg.LogRetentionDays)
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota = %d, want 0 (oss unlimited)", cfg.DefaultEventQuota)
	}
	if cfg.MaxEventBytes != 1048576 {
		t.Errorf("MaxEventBytes = %d, want 1048576", cfg.MaxEventBytes)
	}
	if cfg.SecretKey != "insecure-dev-secret" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
	if cfg.UptimeConcurrency != 50 {
		t.Errorf("UptimeConcurrency = %d, want 50", cfg.UptimeConcurrency)
	}
	if cfg.LocalRegion != "local" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "local")
	}
	if cfg.ProbeToken != "" {
		t.Errorf("ProbeToken = %q, want empty", cfg.ProbeToken)
	}
	if cfg.ServerURL != "" {
		t.Errorf("ServerURL = %q, want empty", cfg.ServerURL)
	}
	if cfg.AgentDistDir != "/opt/gotcha/agent-dist" {
		t.Errorf("AgentDistDir = %q, want /opt/gotcha/agent-dist", cfg.AgentDistDir)
	}
	if cfg.AgentDistRatePerMin != 120 {
		t.Errorf("AgentDistRatePerMin = %d, want 120", cfg.AgentDistRatePerMin)
	}
	if cfg.ExportDir != "/var/lib/gotcha/exports" {
		t.Errorf("ExportDir = %q, want /var/lib/gotcha/exports", cfg.ExportDir)
	}
	if cfg.ExportTTLHours != 168 {
		t.Errorf("ExportTTLHours = %d, want 168", cfg.ExportTTLHours)
	}
	if cfg.ExportMaxRows != 200_000 {
		t.Errorf("ExportMaxRows = %d, want 200000", cfg.ExportMaxRows)
	}
	if cfg.ExportMaxBytes != 268_435_456 {
		t.Errorf("ExportMaxBytes = %d, want 268435456", cfg.ExportMaxBytes)
	}
	if cfg.ExportDiskBudgetBytes != 5_368_709_120 {
		t.Errorf("ExportDiskBudgetBytes = %d, want 5368709120", cfg.ExportDiskBudgetBytes)
	}
}

func TestLoadConfig_ExportOverrides(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EXPORT_DIR":               "/data/exports",
		"GOTCHA_EXPORT_RETENTION_HOURS":   "24",
		"GOTCHA_EXPORT_MAX_ROWS":          "1000",
		"GOTCHA_EXPORT_MAX_BYTES":         "1048576",
		"GOTCHA_EXPORT_DISK_BUDGET_BYTES": "2097152",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ExportDir != "/data/exports" {
		t.Errorf("ExportDir = %q, want /data/exports", cfg.ExportDir)
	}
	if cfg.ExportTTLHours != 24 {
		t.Errorf("ExportTTLHours = %d, want 24", cfg.ExportTTLHours)
	}
	if cfg.ExportMaxRows != 1000 {
		t.Errorf("ExportMaxRows = %d, want 1000", cfg.ExportMaxRows)
	}
	if cfg.ExportMaxBytes != 1048576 {
		t.Errorf("ExportMaxBytes = %d, want 1048576", cfg.ExportMaxBytes)
	}
	if cfg.ExportDiskBudgetBytes != 2097152 {
		t.Errorf("ExportDiskBudgetBytes = %d, want 2097152", cfg.ExportDiskBudgetBytes)
	}
}

// явно заданная пустая строка для str() неотличима от «не задано»
func TestLoadConfig_AgentDistDirEmptyEnvFallsBackToDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DIST_DIR": ""}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentDistDir != "/opt/gotcha/agent-dist" {
		t.Errorf("AgentDistDir = %q, want /opt/gotcha/agent-dist (empty env should fall back to default)", cfg.AgentDistDir)
	}
}

func TestLoadConfig_AgentDistRatePerMinOverride(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DIST_RATE_PER_MIN": "500"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentDistRatePerMin != 500 {
		t.Errorf("AgentDistRatePerMin = %d, want 500", cfg.AgentDistRatePerMin)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := map[string]string{
		"GOTCHA_LISTEN_ADDR":          ":9090",
		"GOTCHA_BASE_URL":             "https://errors.example.com",
		"GOTCHA_PG_DSN":               "postgres://u:p@pg:5432/g",
		"GOTCHA_CH_DSN":               "clickhouse://ch:9000/g",
		"GOTCHA_SMTP_HOST":            "smtp.example.com",
		"GOTCHA_SMTP_PORT":            "465",
		"GOTCHA_SMTP_USER":            "mailer",
		"GOTCHA_SMTP_PASSWORD":        "s3cret",
		"GOTCHA_SMTP_FROM":            "gotcha@example.com",
		"GOTCHA_EVENT_RETENTION_DAYS": "30",
		"GOTCHA_SPAN_RETENTION_DAYS":  "7",
		"GOTCHA_DEFAULT_EVENT_QUOTA":  "50000",
		"GOTCHA_MAX_EVENT_BYTES":      "2097152",
		"GOTCHA_SECRET_KEY":           "prod-secret-at-least-32-bytes-long!",
		"GOTCHA_UPTIME_CONCURRENCY":   "10",
		"GOTCHA_UPTIME_LOCAL_REGION":  "eu-fra",
		"GOTCHA_PROBE_KEY":            "ptok",
		"GOTCHA_PROBE_SERVER_URL":     "https://gotcha.example.com",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode", "ingest"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "ingest" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "ingest")
	}
	if cfg.Addr != ":9090" || cfg.BaseURL != "https://errors.example.com" {
		t.Errorf("Addr/BaseURL not overridden: %q %q", cfg.Addr, cfg.BaseURL)
	}
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPPort != 465 ||
		cfg.SMTPUser != "mailer" || cfg.SMTPPassword != "s3cret" ||
		cfg.SMTPFrom != "gotcha@example.com" {
		t.Errorf("SMTP not overridden: %+v", cfg)
	}
	if cfg.RetentionDays != 30 || cfg.DefaultEventQuota != 50000 || cfg.MaxEventBytes != 2097152 {
		t.Errorf("numeric overrides failed: %+v", cfg)
	}
	if cfg.SpanRetentionDays != 7 {
		t.Errorf("SpanRetentionDays = %d, want 7", cfg.SpanRetentionDays)
	}
	if cfg.SecretKey != "prod-secret-at-least-32-bytes-long!" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
	if cfg.UptimeConcurrency != 10 {
		t.Errorf("UptimeConcurrency = %d, want 10", cfg.UptimeConcurrency)
	}
	if cfg.LocalRegion != "eu-fra" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "eu-fra")
	}
	if cfg.ProbeToken != "ptok" {
		t.Errorf("ProbeToken = %q, want %q", cfg.ProbeToken, "ptok")
	}
	if cfg.ServerURL != "https://gotcha.example.com" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "https://gotcha.example.com")
	}
}

// хвостовая косая снимается: отправитель дописывает «/bot{token}/sendMessage»,
// «…org/» дало бы «…org//bot…» и 404 на каждое уведомление
func TestLoadConfigTelegramAPIBase(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.TelegramAPIBase != "" {
		t.Errorf("default TelegramAPIBase = %q, want empty", cfg.TelegramAPIBase)
	}
	for _, tc := range []struct{ in, want string }{
		{"https://tg.example.com", "https://tg.example.com"},
		{"https://tg.example.com/", "https://tg.example.com"},
		{"https://tg.example.com///", "https://tg.example.com"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"https://gw.example.com/telegram", "https://gw.example.com/telegram"},
	} {
		env := map[string]string{"GOTCHA_TELEGRAM_API_BASE": tc.in}
		cfg, err := loadConfig(getenvFrom(env), nil)
		if err != nil {
			t.Fatalf("GOTCHA_TELEGRAM_API_BASE=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.TelegramAPIBase != tc.want {
			t.Errorf("GOTCHA_TELEGRAM_API_BASE=%q: got %q, want %q", tc.in, cfg.TelegramAPIBase, tc.want)
		}
	}
}

// хвостовая косая снимается: продукт сам дописывает путь к базе (heartbeat,
// OAuth RedirectURI, ссылка-приглашение) — «…app/» дало бы «…app//uptime/hb/…»
func TestLoadConfigBaseURLNormalized(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("default BaseURL = %q, want %q", cfg.BaseURL, "http://localhost:8080")
	}
	for _, tc := range []struct{ in, want string }{
		{"https://gotcha.example.com", "https://gotcha.example.com"},
		{"https://gotcha.example.com/", "https://gotcha.example.com"},
		{"https://gotcha.example.com///", "https://gotcha.example.com"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"https://gw.example.com/gotcha", "https://gw.example.com/gotcha"},
	} {
		// без ALLOW_INSECURE_SECRET тест упёрся бы в чужую проверку — слабый
		// GOTCHA_SECRET_KEY на не-local BaseURL (значения тут не local)
		env := map[string]string{"GOTCHA_BASE_URL": tc.in, "GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1"}
		cfg, err := loadConfig(getenvFrom(env), nil)
		if err != nil {
			t.Fatalf("GOTCHA_BASE_URL=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.BaseURL != tc.want {
			t.Errorf("GOTCHA_BASE_URL=%q: got %q, want %q", tc.in, cfg.BaseURL, tc.want)
		}
	}
}

func TestLoadConfigBaseURLRejectsInvalid(t *testing.T) {
	for _, v := range []string{
		"gotcha.example.com",
		"/app",
		"ftp://gotcha.example.com",
		"https://gotcha.example.com?token=1",
		"https://gotcha.example.com#frag",
		"https://",
	} {
		env := map[string]string{"GOTCHA_BASE_URL": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("GOTCHA_BASE_URL=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigTelegramAPIBaseRejectsInvalid(t *testing.T) {
	for _, v := range []string{
		"tg.example.com",
		"/telegram",
		"ftp://tg.example.com",
		"https://tg.example.com?token=1",
		"https://tg.example.com#frag",
		"https://",
	} {
		env := map[string]string{"GOTCHA_TELEGRAM_API_BASE": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("GOTCHA_TELEGRAM_API_BASE=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigInvalidMode(t *testing.T) {
	if _, err := loadConfig(getenvFrom(nil), []string{"--mode", "banana"}); err == nil {
		t.Fatal("want error for invalid mode, got nil")
	}
}

func TestLoadConfigAcceptsUptimeAndProbeModes(t *testing.T) {
	// заданы для обоих режимов, чтобы проверялся только разбор --mode
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}
	for _, mode := range []string{"uptime", "probe"} {
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode", mode})
		if err != nil {
			t.Fatalf("mode %q: loadConfig: %v", mode, err)
		}
		if cfg.Mode != mode {
			t.Errorf("mode %q: Mode = %q, want %q", mode, cfg.Mode, mode)
		}
	}
}

func TestLoadConfigProbeModeRejectsServerURLWithoutScheme(t *testing.T) {
	for _, serverURL := range []string{"gotcha.example.com", "/probe", "ftp://gotcha.example.com"} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": serverURL,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: want error, got nil", serverURL)
		}
	}
}

// проверяет контракт конфигурации (cfg.ServerURL), не сборку запроса пробы:
// internal/uptime/probeclient.go режет хвостовой слэш сам, независимо от этого теста
func TestLoadConfigProbeServerURLNormalized(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://gotcha.example.com", "https://gotcha.example.com"},
		{"https://gotcha.example.com/", "https://gotcha.example.com"},
		{"https://gotcha.example.com///", "https://gotcha.example.com"},
	} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": tc.in,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"})
		if err != nil {
			t.Fatalf("GOTCHA_PROBE_SERVER_URL=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.ServerURL != tc.want {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: got %q, want %q", tc.in, cfg.ServerURL, tc.want)
		}
	}
}

// формат проверяется только в --mode=probe: вне него переменную никто не
// читает, и ронять по ней старт было бы тихим breaking change
func TestLoadConfigProbeServerURLRejectsQuery(t *testing.T) {
	for _, v := range []string{
		"https://gotcha.example.com?token=1",
		"https://gotcha.example.com#frag",
	} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": v,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigProbeServerURLWarnsOutsideProbeMode(t *testing.T) {
	capture := func(t *testing.T, env map[string]string, args []string) ([]slog.Record, error) {
		t.Helper()
		var records []slog.Record
		prev := slog.Default()
		slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
		defer slog.SetDefault(prev)
		_, err := loadConfig(getenvFrom(env), args)
		return records, err
	}
	hasWarn := func(records []slog.Record, substr string) bool {
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
				return true
			}
		}
		return false
	}

	records, err := capture(t, map[string]string{"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com"}, nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("нет предупреждения о GOTCHA_PROBE_SERVER_URL вне --mode=probe")
	}

	records, err = capture(t, map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}, []string{"--mode", "probe"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("предупреждение о GOTCHA_PROBE_SERVER_URL выдано в --mode=probe, где переменная используется")
	}

	records, err = capture(t, nil, nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("предупреждение о GOTCHA_PROBE_SERVER_URL выдано при незаданной переменной")
	}
}

// безусловный Normalize вне режима пробы был бы тихим breaking change: оператор
// с опечатанным, но нечитаемым значением перестал бы стартовать
func TestLoadConfigProbeServerURLInvalidOutsideProbeModeOnlyWarns(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prev)

	for _, v := range []string{
		"not-a-url",
		"ftp://gotcha.example.com",
		"https://gotcha.example.com?token=1",
	} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROBE_SERVER_URL": v}), nil)
		if err != nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q вне --mode=probe: want no error, got %v", v, err)
			continue
		}
		if cfg.ServerURL != v {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q вне --mode=probe: ServerURL = %q, want непотронутое значение %q", v, cfg.ServerURL, v)
		}
	}
	found := false
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "GOTCHA_PROBE_SERVER_URL") {
			found = true
			break
		}
	}
	if !found {
		t.Error("нет предупреждения о GOTCHA_PROBE_SERVER_URL для невалидного значения вне --mode=probe")
	}
}

func TestLoadConfigDSNsRejectUnparseable(t *testing.T) {
	cases := []struct {
		key string
		env map[string]string
	}{
		{"GOTCHA_PG_DSN", map[string]string{"GOTCHA_PG_DSN": "::::"}},
		{"GOTCHA_CH_DSN", map[string]string{"GOTCHA_CH_DSN": "::::"}},
	}
	for _, tc := range cases {
		_, err := loadConfig(getenvFrom(tc.env), nil)
		if err == nil {
			t.Errorf("%s=::::: want error, got nil", tc.key)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=::::: error = %q, want it to name %s", tc.key, err, tc.key)
		}
	}
}

// pgxpool.ParseConfig принимает и keyword/value-форму, не только URL-форму
func TestLoadConfigDSNsAcceptKeywordValueForm(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PG_DSN": "host=pg.example port=5432 user=gotcha password=s3cret dbname=gotcha sslmode=disable",
	}
	if _, err := loadConfig(getenvFrom(env), nil); err != nil {
		t.Errorf("keyword/value GOTCHA_PG_DSN: want no error, got %v", err)
	}
}

func TestLoadConfigNonPositiveUptimeConcurrency(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		env := map[string]string{"GOTCHA_UPTIME_CONCURRENCY": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Fatalf("GOTCHA_UPTIME_CONCURRENCY=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigInvalidInt(t *testing.T) {
	env := map[string]string{"GOTCHA_EVENT_RETENTION_DAYS": "ninety"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("want error for non-numeric GOTCHA_EVENT_RETENTION_DAYS, got nil")
	}
}

func TestLoadConfigZeroRetentionMeansForever(t *testing.T) {
	env := map[string]string{
		"GOTCHA_EVENT_RETENTION_DAYS":    "0",
		"GOTCHA_SPAN_RETENTION_DAYS":     "0",
		"GOTCHA_METRIC_RETENTION_DAYS":   "0",
		"GOTCHA_PROFILE_RETENTION_DAYS":  "0",
		"GOTCHA_LOG_RETENTION_DAYS":      "0",
		"GOTCHA_INCIDENT_RETENTION_DAYS": "0",
		"GOTCHA_DEPLOY_RETENTION_DAYS":   "0",
	}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RetentionDays != 0 || cfg.SpanRetentionDays != 0 ||
		cfg.MetricRetentionDays != 0 || cfg.ProfileRetentionDays != 0 ||
		cfg.LogRetentionDays != 0 || cfg.IncidentRetentionDays != 0 ||
		cfg.DeployRetentionDays != 0 {
		t.Fatalf("want all retention fields = 0, got %d/%d/%d/%d/%d/%d/%d",
			cfg.RetentionDays, cfg.SpanRetentionDays, cfg.MetricRetentionDays,
			cfg.ProfileRetentionDays, cfg.LogRetentionDays, cfg.IncidentRetentionDays,
			cfg.DeployRetentionDays)
	}
}

func TestLoadConfigNegativeRetentionRejected(t *testing.T) {
	for _, key := range []string{
		"GOTCHA_EVENT_RETENTION_DAYS",
		"GOTCHA_SPAN_RETENTION_DAYS",
		"GOTCHA_METRIC_RETENTION_DAYS",
		"GOTCHA_PROFILE_RETENTION_DAYS",
		"GOTCHA_LOG_RETENTION_DAYS",
		"GOTCHA_INCIDENT_RETENTION_DAYS",
		"GOTCHA_DEPLOY_RETENTION_DAYS",
	} {
		env := map[string]string{key: "-1"}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("%s=-1: want error, got nil", key)
		}
	}
}

func TestLoadConfigNonPositiveDefaultEventQuota(t *testing.T) {
	env := map[string]string{"GOTCHA_DEFAULT_EVENT_QUOTA": "-1"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("GOTCHA_DEFAULT_EVENT_QUOTA=-1: want error, got nil")
	}
}

func TestLoadConfigNonPositiveMaxEventBytes(t *testing.T) {
	env := map[string]string{"GOTCHA_MAX_EVENT_BYTES": "-1"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("GOTCHA_MAX_EVENT_BYTES=-1: want error, got nil")
	}
}

func TestLoadConfigProbeModeRequiresServerURLAndToken(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"both missing", nil},
		{"no token", map[string]string{"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com"}},
		{"no server url", map[string]string{"GOTCHA_PROBE_KEY": "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConfig(getenvFrom(tc.env), []string{"--mode=probe"}); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestLoadConfigProbeMode(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=probe"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "probe" {
		t.Errorf("Mode = %q, want probe", cfg.Mode)
	}
	if cfg.ServerURL != "https://gotcha.example.com" || cfg.ProbeToken != "probe-token" {
		t.Errorf("ServerURL = %q, ProbeToken set = %v", cfg.ServerURL, cfg.ProbeToken != "")
	}
}

func TestLoadConfigNonProbeModeDoesNotRequireProbeCreds(t *testing.T) {
	for _, mode := range []string{"ingest", "web", "uptime", "all"} {
		if _, err := loadConfig(getenvFrom(nil), []string{"--mode=" + mode}); err != nil {
			t.Errorf("--mode=%s: %v", mode, err)
		}
	}
}

func TestLoadConfigOAuthProviders(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":         "true",
		"GOTCHA_OIDC_ISSUER":          "https://idp.example/realms/x",
		"GOTCHA_OIDC_CLIENT_ID":       "cid",
		"GOTCHA_OIDC_CLIENT_SECRET":   "sec",
		"GOTCHA_OIDC_DISPLAY_NAME":    "Corp SSO",
		"GOTCHA_YANDEX_ENABLED":       "true",
		"GOTCHA_YANDEX_CLIENT_ID":     "ycid",
		"GOTCHA_YANDEX_CLIENT_SECRET": "ysec",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.OIDCEnabled || cfg.OIDCIssuer != "https://idp.example/realms/x" || cfg.OIDCName != "Corp SSO" {
		t.Fatalf("OIDC fields = %+v", cfg)
	}
	if !cfg.YandexEnabled || cfg.YandexClientID != "ycid" {
		t.Fatalf("Yandex fields = %+v", cfg)
	}
	if cfg.VKEnabled {
		t.Fatalf("VK must be disabled")
	}
}

func TestLoadConfigOIDCTrustEmailDefaultsFalse(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":       "true",
		"GOTCHA_OIDC_ISSUER":        "https://idp.example",
		"GOTCHA_OIDC_CLIENT_ID":     "cid",
		"GOTCHA_OIDC_CLIENT_SECRET": "sec",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.OIDCTrustEmail {
		t.Fatal("GOTCHA_OIDC_TRUST_EMAIL must default to false (fail-closed)")
	}
}

func TestLoadConfigOIDCTrustEmailEnabled(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":       "true",
		"GOTCHA_OIDC_ISSUER":        "https://idp.example",
		"GOTCHA_OIDC_CLIENT_ID":     "cid",
		"GOTCHA_OIDC_CLIENT_SECRET": "sec",
		"GOTCHA_OIDC_TRUST_EMAIL":   "true",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.OIDCTrustEmail {
		t.Fatal("GOTCHA_OIDC_TRUST_EMAIL=true must set cfg.OIDCTrustEmail")
	}
}

func TestLoadConfigOIDCTrustEmailWarnsWhenUntrusted(t *testing.T) {
	baseEnv := map[string]string{
		"GOTCHA_OIDC_ENABLED":       "true",
		"GOTCHA_OIDC_ISSUER":        "https://idp.example",
		"GOTCHA_OIDC_CLIENT_ID":     "cid",
		"GOTCHA_OIDC_CLIENT_SECRET": "sec",
	}
	hasWarn := func(records []slog.Record) bool {
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, "GOTCHA_OIDC_TRUST_EMAIL") {
				return true
			}
		}
		return false
	}

	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	if _, err := loadConfig(getenvFrom(baseEnv), []string{"--mode=web"}); err != nil {
		slog.SetDefault(prev)
		t.Fatalf("loadConfig: %v", err)
	}
	slog.SetDefault(prev)
	if !hasWarn(records) {
		t.Error("нет предупреждения о GOTCHA_OIDC_TRUST_EMAIL при включённом OIDC без доверия")
	}

	trustedEnv := map[string]string{}
	for k, v := range baseEnv {
		trustedEnv[k] = v
	}
	trustedEnv["GOTCHA_OIDC_TRUST_EMAIL"] = "true"
	records = nil
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	if _, err := loadConfig(getenvFrom(trustedEnv), []string{"--mode=web"}); err != nil {
		slog.SetDefault(prev)
		t.Fatalf("loadConfig: %v", err)
	}
	slog.SetDefault(prev)
	if hasWarn(records) {
		t.Error("предупреждение о GOTCHA_OIDC_TRUST_EMAIL выдано, хотя доверие включено")
	}
}

func TestLoadConfigOAuthMissingSecretFails(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":   "true",
		"GOTCHA_OIDC_ISSUER":    "https://idp.example",
		"GOTCHA_OIDC_CLIENT_ID": "cid",
	}
	if _, err := loadConfig(getenvFrom(env), []string{"--mode=all"}); err == nil {
		t.Fatal("enabled OIDC without secret must fail at startup")
	}
}

func TestLoadConfigProfileDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProfileRetentionDays != 7 {
		t.Errorf("ProfileRetentionDays = %d, want 7", cfg.ProfileRetentionDays)
	}
	if cfg.DefaultProfileQuota != 0 {
		t.Errorf("DefaultProfileQuota = %d, want 0 (oss unlimited)", cfg.DefaultProfileQuota)
	}
}

func TestLoadConfigIngestRateLimit(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.IngestRateLimit != 500 {
		t.Fatalf("default IngestRateLimit = %d, want 500", cfg.IngestRateLimit)
	}
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_RATE_PER_SEC": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig with 0: %v", err)
	}
	if cfg.IngestRateLimit != 0 {
		t.Fatalf("IngestRateLimit = %d, want 0", cfg.IngestRateLimit)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_RATE_PER_SEC": "-1"}), nil); err == nil {
		t.Fatal("GOTCHA_INGEST_RATE_PER_SEC=-1: want error, got nil")
	}
}

func TestLoadConfigPreAuthRateLimit(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.PreAuthRateLimit != 2000 {
		t.Fatalf("default PreAuthRateLimit = %d, want 2000", cfg.PreAuthRateLimit)
	}
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_PREAUTH_RATE_PER_SEC": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig with 0: %v", err)
	}
	if cfg.PreAuthRateLimit != 0 {
		t.Fatalf("PreAuthRateLimit = %d, want 0", cfg.PreAuthRateLimit)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_PREAUTH_RATE_PER_SEC": "-1"}), nil); err == nil {
		t.Fatal("GOTCHA_INGEST_PREAUTH_RATE_PER_SEC=-1: want error, got nil")
	}
}

func TestLoadConfigSignalTouchRateLimit(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SignalTouchRateLimit != 2 {
		t.Fatalf("default SignalTouchRateLimit = %d, want 2", cfg.SignalTouchRateLimit)
	}
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_SIGNAL_RATE_PER_SEC": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig with 0: %v", err)
	}
	if cfg.SignalTouchRateLimit != 0 {
		t.Fatalf("SignalTouchRateLimit = %d, want 0", cfg.SignalTouchRateLimit)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_SIGNAL_RATE_PER_SEC": "-1"}), nil); err == nil {
		t.Fatal("GOTCHA_INGEST_SIGNAL_RATE_PER_SEC=-1: want error, got nil")
	}
}

func TestLoadConfigOutboxRetention(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.OutboxRetentionDays != 7 {
		t.Errorf("OutboxRetentionDays = %d, want 7", cfg.OutboxRetentionDays)
	}
	// outbox — рабочая очередь, не архив: «хранить вечно» тут был бы неограниченный рост
	for _, v := range []string{"0", "-1"} {
		if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_OUTBOX_RETENTION_DAYS": v}), nil); err == nil {
			t.Errorf("GOTCHA_OUTBOX_RETENTION_DAYS=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfig_RejectsDefaultSecretInProd(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL": "https://gotcha.example.com",
	}
	getenv := func(k string) string { return env[k] }
	_, err := loadConfig(getenv, []string{"--mode=all"})
	if err == nil {
		t.Fatal("expected error for default secret on non-local prod base url, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY") {
		t.Fatalf("error should mention GOTCHA_SECRET_KEY, got: %v", err)
	}
}

func TestLoadConfig_AllowsDefaultSecretOnLocalhost(t *testing.T) {
	getenv := func(k string) string { return "" } // всё дефолтное, BaseURL=localhost
	if _, err := loadConfig(getenv, []string{"--mode=all"}); err != nil {
		t.Fatalf("localhost dev must be allowed with default secret, got: %v", err)
	}
}

func TestLoadConfig_AllowsDefaultSecretWithEscapeHatch(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}
	getenv := func(k string) string { return env[k] }
	if _, err := loadConfig(getenv, []string{"--mode=all"}); err != nil {
		t.Fatalf("escape hatch must allow default secret, got: %v", err)
	}
}

func TestLoadConfig_SecretRequiredInDecryptingModes(t *testing.T) {
	for _, mode := range []string{"web", "all", "ingest", "uptime"} {
		env := map[string]string{"GOTCHA_BASE_URL": "https://gotcha.example.com"}
		getenv := func(k string) string { return env[k] }
		if _, err := loadConfig(getenv, []string{"--mode=" + mode}); err == nil {
			t.Errorf("режим %s стартовал с дефолтным ключом на не-локальном URL", mode)
		}
	}

	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "ptok",
	}
	getenv := func(k string) string { return env[k] }
	if _, err := loadConfig(getenv, []string{"--mode=probe"}); err != nil {
		t.Errorf("probe не расшифровывает секреты, ключ не нужен: %v", err)
	}
}

func TestLoadConfig_SecretErrorNotMaskedByUnrelatedTypo(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":             "https://gotcha.example.com",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc",
	}
	getenv := func(k string) string { return env[k] }
	_, err := loadConfig(getenv, []string{"--mode=all"})
	if err == nil {
		t.Fatal("expected error (both a numeric typo and a weak secret are present), got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY") {
		t.Fatalf("the security-relevant secret error must surface even with an unrelated config typo present, got: %v", err)
	}
}

func TestLoadConfig_SecretKeyPrevRejectsInconsistentPairs(t *testing.T) {
	const strongCurrent = "current-master-key-at-least-32-bytes!!"
	const strongOther = "other-master-key-also-32-bytes-long!!!"

	t.Run("PREV равен дев-ключу", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": devSecretKey,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("PREV == дев-ключ: want error, got nil")
		}
		// фрагмент уникален для этой ветки: переклейка текстов ошибок между ветками уронит тест
		if !strings.Contains(err.Error(), "public dev default") {
			t.Errorf("error = %q, want it to mention that PREV cannot be the public dev default", err)
		}
	})

	t.Run("PREV равен текущему ключу", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": strongCurrent,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("PREV == текущий ключ: want error, got nil")
		}
		if !strings.Contains(err.Error(), "differ from GOTCHA_SECRET_KEY") {
			t.Errorf("error = %q, want it to mention that PREV must differ from GOTCHA_SECRET_KEY", err)
		}
	})

	t.Run("текущий ключ дев, PREV задан", func(t *testing.T) {
		// localhost — чтобы не упереться в чужую проверку (дефолтный ключ на не-локальном URL)
		env := map[string]string{
			"GOTCHA_BASE_URL":        "http://localhost:8080",
			"GOTCHA_SECRET_KEY_PREV": strongOther,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("текущий ключ дев + PREV задан: want error, got nil")
		}
		if !strings.Contains(err.Error(), "still the dev default") {
			t.Errorf("error = %q, want it to mention that GOTCHA_SECRET_KEY is still the dev default", err)
		}
	})

	t.Run("корректная пара стартует", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": strongOther,
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("корректная пара ключей должна стартовать: %v", err)
		}
		if cfg.SecretKeyPrev != strongOther {
			t.Errorf("SecretKeyPrev = %q, want %q", cfg.SecretKeyPrev, strongOther)
		}
	})

	t.Run("пустой PREV — норма", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":   "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY": strongCurrent,
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("пустой PREV — ротации нет, старт должен пройти: %v", err)
		}
		if cfg.SecretKeyPrev != "" {
			t.Errorf("SecretKeyPrev = %q, want пусто", cfg.SecretKeyPrev)
		}
	})

	t.Run("короткий PREV — норма, порога нет", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "http://localhost:8080",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": "short-prev-key",
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("короткий PREV не про стойкость: порога длины нет: %v", err)
		}
		if cfg.SecretKeyPrev != "short-prev-key" {
			t.Errorf("SecretKeyPrev = %q, want short-prev-key", cfg.SecretKeyPrev)
		}
	})
}

// PREV читается дословно, без обрезки пробелов, в отличие от SecretKey (strGuarded)
func TestLoadConfig_SecretKeyPrevReadVerbatimNotTrimmed(t *testing.T) {
	const key = "current-master-key-at-least-32-bytes!!"
	env := map[string]string{
		"GOTCHA_BASE_URL":        "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY":      key + " ",
		"GOTCHA_SECRET_KEY_PREV": key + " ",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("PREV с тем же хвостовым пробелом, что был у ключа до тримминга: want старт, got error: %v", err)
	}
	if cfg.SecretKey != key {
		t.Errorf("SecretKey = %q, want %q (текущий ключ по-прежнему триммится)", cfg.SecretKey, key)
	}
	if cfg.SecretKeyPrev != key+" " {
		t.Errorf("SecretKeyPrev = %q, want %q (читается дословно, без тримминга)", cfg.SecretKeyPrev, key+" ")
	}
}

// ALLOW_INSECURE снимает требования к стойкости ключа, а не к логической
// согласованности пары current/PREV
func TestLoadConfig_SecretKeyPrevAllowInsecureDoesNotBypass(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_PREV":           "some-other-strong-previous-key-32b!!",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err == nil {
		t.Fatal("GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 не должен снимать проверку PREV: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_PREV") {
		t.Errorf("error = %q, want it to mention GOTCHA_SECRET_KEY_PREV", err)
	}
}

func TestLoadConfig_SecretKeyPrevNotCheckedInProbeMode(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "ptok",
		// в любом другом режиме PREV == dev-ключ — отказ старта
		"GOTCHA_SECRET_KEY_PREV": devSecretKey,
	}
	if _, err := loadConfig(getenvFrom(env), []string{"--mode=probe"}); err != nil {
		t.Fatalf("probe не проверяет PREV, ключ ему не нужен: %v", err)
	}
}

func TestLoadConfig_SecretKeyPrevWhitespaceOnlyRejected(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":        "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY":      "current-master-key-at-least-32-bytes!!",
		"GOTCHA_SECRET_KEY_PREV": "   ",
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err == nil {
		t.Fatal("GOTCHA_SECRET_KEY_PREV=\"   \": want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_PREV") || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error = %q, want it to name GOTCHA_SECRET_KEY_PREV and say 'whitespace'", err)
	}
}

// проверка пробельности PREV не под secretKeyMattersFor: та же безусловная
// гигиена значения, что у strGuarded(), действует независимо от режима
func TestLoadConfig_SecretKeyPrevWhitespaceOnlyRejectedEvenInProbeMode(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "ptok",
		"GOTCHA_SECRET_KEY_PREV":  "   ",
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=probe"})
	if err == nil {
		t.Fatal("GOTCHA_SECRET_KEY_PREV=\"   \" в probe: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_PREV") {
		t.Errorf("error = %q, want it to name GOTCHA_SECRET_KEY_PREV", err)
	}
}

func TestLoadConfigProfileEvalInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProfileEvalInterval != 300 {
		t.Errorf("ProfileEvalInterval = %d, want 300", cfg.ProfileEvalInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero profile eval interval must fail")
	}
}

func TestLoadConfigHostEvalInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HostEvalInterval != 60 {
		t.Errorf("HostEvalInterval = %d, want 60", cfg.HostEvalInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_HOST_EVAL_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero host eval interval must fail")
	}
}

func TestLoadConfigEscalationInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.EscalationInterval != 60 {
		t.Errorf("EscalationInterval = %d, want 60", cfg.EscalationInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_ESCALATION_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero escalation interval must fail")
	}
}

func TestLoadConfig_Registration(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RegistrationMode != "invite" {
		t.Errorf("RegistrationMode default = %q, want %q", cfg.RegistrationMode, "invite")
	}
	for _, mode := range []string{"open", "invite", "closed"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION_MODE": mode}), nil)
		if err != nil {
			t.Fatalf("loadConfig %q: %v", mode, err)
		}
		if cfg.RegistrationMode != mode {
			t.Errorf("RegistrationMode = %q, want %q", cfg.RegistrationMode, mode)
		}
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION_MODE": "bogus"}), nil); err == nil {
		t.Error("bogus registration mode must fail")
	}
}

func TestLoadConfig_Locale(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Locale != "ru" {
		t.Errorf("Locale default = %q, want %q", cfg.Locale, "ru")
	}
	for _, loc := range []string{"ru", "en"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_LOCALE": loc}), nil)
		if err != nil {
			t.Fatalf("loadConfig %q: %v", loc, err)
		}
		if cfg.Locale != loc {
			t.Errorf("Locale = %q, want %q", cfg.Locale, loc)
		}
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_LOCALE": "de"}), nil); err == nil {
		t.Error("bogus locale must fail")
	}
}

func TestLoadConfig_Edition(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Edition != "oss" {
		t.Errorf("Edition default = %q, want %q", cfg.Edition, "oss")
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota (oss) = %d, want 0", cfg.DefaultEventQuota)
	}
	if cfg.DefaultTransactionQuota != 0 || cfg.DefaultMetricQuota != 0 || cfg.DefaultProfileQuota != 0 || cfg.DefaultLogQuota != 0 {
		t.Errorf("oss quotas not all 0: tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "saas"}), nil)
	if err != nil {
		t.Fatalf("loadConfig saas: %v", err)
	}
	if cfg.Edition != "saas" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "saas")
	}
	if cfg.DefaultEventQuota != 1_000_000 {
		t.Errorf("DefaultEventQuota (saas) = %d, want 1000000", cfg.DefaultEventQuota)
	}
	if cfg.DefaultTransactionQuota != 1_000_000 || cfg.DefaultMetricQuota != 1_000_000 || cfg.DefaultProfileQuota != 1_000_000 || cfg.DefaultLogQuota != 1_000_000 {
		t.Errorf("saas quotas not all 1000000: tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION":             "saas",
		"GOTCHA_DEFAULT_EVENT_QUOTA": "0",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig saas+0: unlimited must be allowed, got: %v", err)
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota = %d, want 0", cfg.DefaultEventQuota)
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_DEFAULT_EVENT_QUOTA":       "10",
		"GOTCHA_DEFAULT_TRANSACTION_QUOTA": "20",
		"GOTCHA_DEFAULT_METRIC_QUOTA":      "30",
		"GOTCHA_DEFAULT_PROFILE_QUOTA":     "40",
		"GOTCHA_DEFAULT_LOG_QUOTA":         "50",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig overrides: %v", err)
	}
	if cfg.DefaultEventQuota != 10 || cfg.DefaultTransactionQuota != 20 ||
		cfg.DefaultMetricQuota != 30 || cfg.DefaultProfileQuota != 40 || cfg.DefaultLogQuota != 50 {
		t.Errorf("quota overrides failed: event=%d tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultEventQuota, cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DEFAULT_METRIC_QUOTA": "-1"}), nil); err == nil {
		t.Error("negative GOTCHA_DEFAULT_METRIC_QUOTA must fail")
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DEFAULT_LOG_QUOTA": "-1"}), nil); err == nil {
		t.Error("negative GOTCHA_DEFAULT_LOG_QUOTA must fail")
	}

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "bogus"}), nil); err == nil {
		t.Error("bogus GOTCHA_EDITION must fail")
	}
}

func TestLoadConfig_Scrub(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.ScrubIP {
		t.Error("ScrubIP default = false, want true")
	}
	if !cfg.ScrubEmail {
		t.Error("ScrubEmail default = false, want true")
	}
	if len(cfg.ScrubKeys) == 0 {
		t.Error("ScrubKeys default is empty, want non-empty")
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_IP": "false"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ScrubIP {
		t.Error("ScrubIP = true с GOTCHA_SCRUB_IP=false, want false")
	}
	if !cfg.ScrubEmail {
		t.Error("ScrubEmail не должен зависеть от GOTCHA_SCRUB_IP")
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_DENY_KEYS": "a,b"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasAll(cfg.ScrubKeys, defaultScrubKeys()) {
		t.Errorf("ScrubKeys = %v, дефолтный denylist потерян", cfg.ScrubKeys)
	}
	if !hasAll(cfg.ScrubKeys, []string{"a", "b"}) {
		t.Errorf("ScrubKeys = %v, пользовательские ключи не добавлены", cfg.ScrubKeys)
	}

	// ",," не должно обнулять denylist: все элементы пустые, ветка с дефолтами не пропускается
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_DENY_KEYS": ",,"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasAll(cfg.ScrubKeys, defaultScrubKeys()) {
		t.Errorf("ScrubKeys = %v при GOTCHA_SCRUB_DENY_KEYS=\",,\" — denylist обнулён", cfg.ScrubKeys)
	}
}

func hasAll(got, want []string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestTrustedRecipientsParsed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_TRUSTED_RECIPIENTS": " Corp.Example , ,ops.example,",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"corp.example", "ops.example"}
	if len(cfg.TrustedRecipients) != len(want) {
		t.Fatalf("TrustedRecipients = %v, want %v", cfg.TrustedRecipients, want)
	}
	for i, w := range want {
		if cfg.TrustedRecipients[i] != w {
			t.Errorf("TrustedRecipients[%d] = %q, want %q", i, cfg.TrustedRecipients[i], w)
		}
	}
}

func TestTrustedRecipientsEmptyByDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.TrustedRecipients) != 0 {
		t.Fatalf("TrustedRecipients = %v, want empty", cfg.TrustedRecipients)
	}
}

func TestMigrateOnlyImpliesAutoMigrate(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_AUTO_MIGRATE_ENABLED": "false"}), []string{"--migrate-only"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.MigrateOnly {
		t.Error("MigrateOnly = false, want true")
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate = false при --migrate-only: запуск не применит миграции")
	}
}

func TestMigrateOnlyRejectedForProbe(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_KEY": "t",
	}), []string{"--migrate-only", "--mode=probe"})
	if err == nil {
		t.Fatal("loadConfig принял --migrate-only с --mode=probe")
	}
	if !strings.Contains(err.Error(), "migrate-only") {
		t.Errorf("ошибка = %v, want упоминание --migrate-only", err)
	}
}

func TestMigrateOnlyDefaultsOff(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateOnly {
		t.Error("MigrateOnly = true без флага")
	}
}

func TestMigrateForceFlags(t *testing.T) {
	probeEnv := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_KEY": "t",
	}
	rejected := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"с probe: база не открывается", probeEnv, []string{"--migrate-force=57", "--mode=probe"}},
		{"с migrate-only: разные намерения", nil, []string{"--migrate-force=57", "--migrate-only"}},
		{"оба разом: застрять могла одна база", nil, []string{"--migrate-force=57", "--migrate-force-ch=3"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(tc.env), tc.args)
			if err == nil {
				t.Fatalf("loadConfig принял %v", tc.args)
			}
			if !strings.Contains(err.Error(), "migrate-force") {
				t.Errorf("ошибка = %v, want упоминание --migrate-force", err)
			}
		})
	}

	cfg, err := loadConfig(getenvFrom(nil), []string{"--migrate-force-ch=12"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateForceCH != 12 || cfg.MigrateForcePG != -1 {
		t.Errorf("MigrateForceCH=%d MigrateForcePG=%d, want 12 и -1", cfg.MigrateForceCH, cfg.MigrateForcePG)
	}

	cfg, err = loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateForcePG != -1 || cfg.MigrateForceCH != -1 {
		t.Errorf("дефолт: MigrateForcePG=%d MigrateForceCH=%d, want -1 и -1", cfg.MigrateForcePG, cfg.MigrateForceCH)
	}
}

// проверка не только «отказ есть», но и «отказ про эту переменную»: сообщения
// писались копипастой соседнего блока
func TestLoadConfigOutOfRangeNumericEnvRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_ALERT_BUDGET_WINDOW_SECONDS", "0"},
		{"GOTCHA_ALERT_BUDGET_LIMIT", "-1"},
		{"GOTCHA_CARDINALITY_LIMIT", "-1"},
		{"GOTCHA_CARDINALITY_WINDOW_SECONDS", "0"},
		{"GOTCHA_METRIC_EVAL_INTERVAL_SECONDS", "0"},
		{"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS", "-1"},
		{"GOTCHA_NOTIFY_CONCURRENCY", "0"},
		{"GOTCHA_SLO_EVAL_INTERVAL_SECONDS", "0"},
		{"GOTCHA_DEPENDENCY_SETTLE_SECONDS", "-1"},
		{"GOTCHA_DEFAULT_TRANSACTION_QUOTA", "-1"},
		{"GOTCHA_DEFAULT_PROFILE_QUOTA", "-1"},
		{"GOTCHA_SMTP_PORT", "-1"},
		{"GOTCHA_SMTP_PORT", "0"},
		{"GOTCHA_SMTP_PORT", "99999"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidBoolean(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_IP": "maybe"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_SCRUB_IP=maybe: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SCRUB_IP") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_SCRUB_IP and say 'invalid boolean'", err)
	}
}

// «8MiB» разбирается в 0, а 0 для GOTCHA_MAX_WRITER_BUFFER_BYTES значит «выведи
// потолок сам» — опечатка тихо меняла бы смысл настройки вместо отказа старта
func TestLoadConfigRejectsNonNumericInt64(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_MAX_WRITER_BUFFER_BYTES": "8MiB"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_MAX_WRITER_BUFFER_BYTES=8MiB: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_WRITER_BUFFER_BYTES") {
		t.Errorf("error = %q, want it to name GOTCHA_MAX_WRITER_BUFFER_BYTES", err)
	}
}

// проверяет parseInt64Env напрямую: разница между def и частичным результатом
// strconv.ParseInt снаружи loadConfig ненаблюдаема ни одним тестом на Config
func TestParseInt64EnvReturnsDefOnParseError(t *testing.T) {
	cases := []struct{ name, value string }{
		{"syntax", "8MiB"},
		{"overflow", "999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GOTCHA_TEST_INT64": tc.value}
			got, err := parseInt64Env(getenvFrom(env), "GOTCHA_TEST_INT64", 5)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.value)
			}
			if !strings.Contains(err.Error(), "GOTCHA_TEST_INT64") {
				t.Errorf("error = %q, want it to name GOTCHA_TEST_INT64", err)
			}
			if got != 5 {
				t.Errorf("got = %d, want def=5 (not a partial strconv.ParseInt result)", got)
			}
		})
	}
	if got, err := parseInt64Env(getenvFrom(nil), "GOTCHA_TEST_INT64", 5); err != nil || got != 5 {
		t.Errorf("unset: got (%d, %v), want (5, nil)", got, err)
	}
	if got, err := parseInt64Env(getenvFrom(map[string]string{"GOTCHA_TEST_INT64": "42"}), "GOTCHA_TEST_INT64", 5); err != nil || got != 42 {
		t.Errorf("valid: got (%d, %v), want (42, nil)", got, err)
	}
}

func TestParseIntEnvReturnsDefOnParseError(t *testing.T) {
	cases := []struct{ name, value string }{
		{"syntax", "8MiB"},
		{"overflow", "999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GOTCHA_TEST_INT": tc.value}
			got, err := parseIntEnv(getenvFrom(env), "GOTCHA_TEST_INT", 7)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.value)
			}
			if !strings.Contains(err.Error(), "GOTCHA_TEST_INT") {
				t.Errorf("error = %q, want it to name GOTCHA_TEST_INT", err)
			}
			if got != 7 {
				t.Errorf("got = %d, want def=7 (not a partial strconv.ParseInt result)", got)
			}
		})
	}
	if got, err := parseIntEnv(getenvFrom(nil), "GOTCHA_TEST_INT", 7); err != nil || got != 7 {
		t.Errorf("unset: got (%d, %v), want (7, nil)", got, err)
	}
	if got, err := parseIntEnv(getenvFrom(map[string]string{"GOTCHA_TEST_INT": "13"}), "GOTCHA_TEST_INT", 7); err != nil || got != 13 {
		t.Errorf("valid: got (%d, %v), want (13, nil)", got, err)
	}
}

func TestParseInt64EnvTrimsSpace(t *testing.T) {
	env := map[string]string{"GOTCHA_TEST_INT64": " 30"}
	got, err := parseInt64Env(getenvFrom(env), "GOTCHA_TEST_INT64", 5)
	if err != nil {
		t.Fatalf("\" 30\": want no error, got %v", err)
	}
	if got != 30 {
		t.Errorf("got = %d, want 30", got)
	}
	if got, err := parseInt64Env(getenvFrom(map[string]string{"GOTCHA_TEST_INT64": "   "}), "GOTCHA_TEST_INT64", 5); err != nil || got != 5 {
		t.Errorf("пробелы: got (%d, %v), want (5, nil)", got, err)
	}
}

func TestParseIntEnvTrimsSpace(t *testing.T) {
	env := map[string]string{"GOTCHA_TEST_INT": " 30 "}
	got, err := parseIntEnv(getenvFrom(env), "GOTCHA_TEST_INT", 7)
	if err != nil {
		t.Fatalf("\" 30 \": want no error, got %v", err)
	}
	if got != 30 {
		t.Errorf("got = %d, want 30", got)
	}
}

func TestLoadConfigNumericEnvTrimmedThroughLoadConfig(t *testing.T) {
	env := map[string]string{"GOTCHA_EVENT_RETENTION_DAYS": " 45 "}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("GOTCHA_EVENT_RETENTION_DAYS=\" 45 \": want no error, got %v", err)
	}
	if cfg.RetentionDays != 45 {
		t.Errorf("RetentionDays = %d, want 45", cfg.RetentionDays)
	}
}

func TestLoadConfigMaxBufferAndQueueBytesZeroOrNegativeRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_MAX_WRITER_BUFFER_BYTES", "0"},
		{"GOTCHA_MAX_WRITER_BUFFER_BYTES", "-1"},
		{"GOTCHA_MAX_INGEST_QUEUE_BYTES", "0"},
		{"GOTCHA_MAX_INGEST_QUEUE_BYTES", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
		})
	}
}

func TestLoadConfigMaxBufferAndQueueBytesUnsetUsesPackageDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := effectiveMaxBufferBytes(cfg.MaxBufferBytes, 0); got != 0 {
		t.Errorf("effectiveMaxBufferBytes(cfg.MaxBufferBytes, 0) = %d, want 0 (writer package default, no heap ceiling detected)", got)
	}
	if cfg.MaxQueueBytes != 0 {
		t.Errorf("MaxQueueBytes = %d, want 0 (queue package default) когда GOTCHA_MAX_INGEST_QUEUE_BYTES не задана", cfg.MaxQueueBytes)
	}
}

// errs[0] вместо errors.Join(errs...) красит именно этот тест: в тексте
// ошибки исчезнет одно из двух имён
func TestLoadConfigGarbageInMultipleNumericVarsReportsAllNames(t *testing.T) {
	env := map[string]string{
		"GOTCHA_MAX_WRITER_BUFFER_BYTES": "8MiB",
		"GOTCHA_MAX_INGEST_QUEUE_BYTES":  "not-a-number",
	}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("мусор в двух числовых переменных: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_WRITER_BUFFER_BYTES") {
		t.Errorf("error = %q, does not name GOTCHA_MAX_WRITER_BUFFER_BYTES", err)
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_INGEST_QUEUE_BYTES") {
		t.Errorf("error = %q, does not name GOTCHA_MAX_INGEST_QUEUE_BYTES", err)
	}
}

func TestLoadConfigDSNAndBaseURLErrorsJoinRestOfErrs(t *testing.T) {
	env := map[string]string{
		"GOTCHA_SCRUB_IP":             "ture",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc",
		"GOTCHA_PG_DSN":               "::::not a dsn",
	}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("битый DSN плюс две опечатки: want error, got nil")
	}
	for _, want := range []string{"GOTCHA_SCRUB_IP", "GOTCHA_EVENT_RETENTION_DAYS", "GOTCHA_PG_DSN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, does not name %s — DSN-ошибка не должна топить остальные накопленные errs", err, want)
		}
	}
}

// cfg.BaseURL остаётся сырым до возврата ошибки; isLocalBaseURL на невалидной
// строке консервативно не совпадает ни с одним локальным хостом
func TestLoadConfigBaseURLErrorJoinsRestOfErrsWithRawValueKept(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":             "http://[::1",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc",
	}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("битый BASE_URL плюс опечатка в числовой: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_BASE_URL") {
		t.Errorf("error = %q, does not name GOTCHA_BASE_URL", err)
	}
	if !strings.Contains(err.Error(), "GOTCHA_EVENT_RETENTION_DAYS") {
		t.Errorf("error = %q, does not name GOTCHA_EVENT_RETENTION_DAYS — BASE_URL-ошибка не должна топить остальные накопленные errs", err)
	}
}

func TestLoadConfigSecretKeyErrorNotDrownedByNumericErrors(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":             "https://gotcha.example",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc",
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err == nil {
		t.Fatal("дефолтный GOTCHA_SECRET_KEY на не-локальном BaseURL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY must be set to a strong random value") {
		t.Errorf("error = %q, want the secret-key warning to be visible and not superseded by the retention-days typo", err)
	}
	if strings.Contains(err.Error(), "GOTCHA_EVENT_RETENTION_DAYS") {
		t.Errorf("error = %q, want ONLY the secret-key warning, not it plus the numeric typo (SEC-C1: secret must win, not share the line)", err)
	}
}

// голый IP обязан становиться /32 (/128 для IPv6), иначе net.ParseCIDR отвергнет
// запись, и самая частая форма («192.168.1.5») стала бы ошибкой конфигурации
func TestLoadConfigTrustedProxiesParsed(t *testing.T) {
	env := map[string]string{
		"GOTCHA_TRUSTED_PROXIES": " 10.0.0.0/8 , 192.168.1.5 ,, 2001:db8::1 ",
	}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.TrustedProxies) != 3 {
		t.Fatalf("TrustedProxies = %v, want 3 entries (пустая запись отбрасывается)", cfg.TrustedProxies)
	}
	got := make([]string, 0, len(cfg.TrustedProxies))
	for _, n := range cfg.TrustedProxies {
		got = append(got, n.String())
	}
	want := []string{"10.0.0.0/8", "192.168.1.5/32", "2001:db8::1/128"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestLoadConfigTrustedProxiesRejectsInvalidEntry(t *testing.T) {
	env := map[string]string{"GOTCHA_TRUSTED_PROXIES": "10.0.0.0/8,not-an-ip"}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("GOTCHA_TRUSTED_PROXIES с мусорной записью: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_TRUSTED_PROXIES") || !strings.Contains(err.Error(), "not-an-ip") {
		t.Errorf("error = %q, want it to name the variable and the bad entry", err)
	}
}

// без нормализации в нижний регистр «Order_ID» не совпал бы ни с чем
func TestLoadConfigScrubAllowKeysParsed(t *testing.T) {
	env := map[string]string{"GOTCHA_SCRUB_KEEP_KEYS": " Order_ID , ,USER_ID"}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.ScrubAllowKeys) != 2 {
		t.Fatalf("ScrubAllowKeys = %v, want 2 entries (пустая запись отбрасывается)", cfg.ScrubAllowKeys)
	}
	if cfg.ScrubAllowKeys[0] != "order_id" || cfg.ScrubAllowKeys[1] != "user_id" {
		t.Errorf("ScrubAllowKeys = %v, want [order_id user_id]", cfg.ScrubAllowKeys)
	}
}

func TestLoadConfigShortSecretKeyOnRemoteBaseURL(t *testing.T) {
	short := "0123456789abcdef" // 16 байт, ровно вдвое меньше требуемого
	base := map[string]string{
		"GOTCHA_BASE_URL":   "https://gotcha.example",
		"GOTCHA_SECRET_KEY": short,
	}
	_, err := loadConfig(getenvFrom(base), []string{"--mode=web"})
	if err == nil {
		t.Fatal("короткий GOTCHA_SECRET_KEY на не-локальном BaseURL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY is too short") {
		t.Errorf("error = %q, want it to say the key is too short", err)
	}

	withEscape := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example",
		"GOTCHA_SECRET_KEY":                short,
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}
	if _, err := loadConfig(getenvFrom(withEscape), []string{"--mode=web"}); err != nil {
		t.Fatalf("GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 должен разрешать короткий ключ: %v", err)
	}

	local := map[string]string{
		"GOTCHA_BASE_URL":   "http://localhost:8080",
		"GOTCHA_SECRET_KEY": short,
	}
	if _, err := loadConfig(getenvFrom(local), []string{"--mode=web"}); err != nil {
		t.Fatalf("короткий ключ на localhost должен проходить: %v", err)
	}

	probe := map[string]string{
		"GOTCHA_BASE_URL":         "https://gotcha.example",
		"GOTCHA_SECRET_KEY":       short,
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example",
		"GOTCHA_PROBE_KEY":        "tok",
	}
	if _, err := loadConfig(getenvFrom(probe), []string{"--mode=probe"}); err != nil {
		t.Fatalf("--mode=probe с коротким ключом должен проходить: %v", err)
	}
}

// ветка отдельная от проверки схемы/хоста: url.Parse возвращает ошибку раньше
func TestLoadConfigRejectsUnparseableURLs(t *testing.T) {
	const broken = "http://[::1"

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_BASE_URL": broken}), nil); err == nil {
		t.Error("GOTCHA_BASE_URL=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_BASE_URL") {
		t.Errorf("BaseURL error = %q, want it to name GOTCHA_BASE_URL", err)
	}

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_TELEGRAM_API_BASE": broken}), nil); err == nil {
		t.Error("GOTCHA_TELEGRAM_API_BASE=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_TELEGRAM_API_BASE") {
		t.Errorf("Telegram error = %q, want it to name GOTCHA_TELEGRAM_API_BASE", err)
	}

	probe := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": broken,
		"GOTCHA_PROBE_KEY":        "tok",
	}
	if _, err := loadConfig(getenvFrom(probe), []string{"--mode=probe"}); err == nil {
		t.Error("GOTCHA_PROBE_SERVER_URL=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_PROBE_SERVER_URL") {
		t.Errorf("probe error = %q, want it to name GOTCHA_PROBE_SERVER_URL", err)
	}
}

func TestLoadConfigSocialProvidersRequireSecrets(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"yandex без id и секрета", map[string]string{"GOTCHA_YANDEX_ENABLED": "1"}},
		{"yandex без секрета", map[string]string{
			"GOTCHA_YANDEX_ENABLED":   "1",
			"GOTCHA_YANDEX_CLIENT_ID": "ycid",
		}},
		{"vk без id и секрета", map[string]string{"GOTCHA_VK_ENABLED": "1"}},
		{"vk без секрета", map[string]string{
			"GOTCHA_VK_ENABLED":   "1",
			"GOTCHA_VK_CLIENT_ID": "vcid",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConfig(getenvFrom(tc.env), []string{"--mode=web"}); err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
		})
	}

	full := map[string]string{
		"GOTCHA_VK_ENABLED":       "1",
		"GOTCHA_VK_CLIENT_ID":     "vcid",
		"GOTCHA_VK_CLIENT_SECRET": "vsec",
	}
	cfg, err := loadConfig(getenvFrom(full), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.VKEnabled || cfg.VKClientSecret != "vsec" {
		t.Errorf("VK fields = %v/%q", cfg.VKEnabled, cfg.VKClientSecret)
	}
}

func TestLoadConfigRunEvaluatorsTriState(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RunEvaluators != nil {
		t.Errorf("RunEvaluators = %v без переменной, want nil", *cfg.RunEvaluators)
	}

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"YES", true}, {"on", true}, {" on ", true},
		{"0", false}, {"false", false}, {"FALSE", false}, {"no", false}, {"NO", false}, {"off", false}, {" off ", false},
	} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EVALUATORS_ENABLED": tc.value}), nil)
		if err != nil {
			t.Fatalf("GOTCHA_EVALUATORS_ENABLED=%q: loadConfig: %v", tc.value, err)
		}
		if cfg.RunEvaluators == nil {
			t.Errorf("GOTCHA_EVALUATORS_ENABLED=%q: RunEvaluators = nil, want заданное значение", tc.value)
			continue
		}
		if *cfg.RunEvaluators != tc.want {
			t.Errorf("GOTCHA_EVALUATORS_ENABLED=%q: RunEvaluators = %v, want %v", tc.value, *cfg.RunEvaluators, tc.want)
		}
	}
}

func TestLoadConfigRunEvaluatorsRejectsInvalid(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EVALUATORS_ENABLED": "ture"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_EVALUATORS_ENABLED=ture: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_EVALUATORS_ENABLED") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_EVALUATORS_ENABLED and say 'invalid boolean'", err)
	}
}

// тесты ниже не пишут старые имена буквально: иначе TestNoRenamedEnvVarNames
// (guards) пришлось бы исключать из проверки весь этот файл, а не точечную фикстуру
func sortedRenamedOldNames() []string {
	names := make([]string, 0, len(envcontract.Renamed))
	for old := range envcontract.Renamed {
		names = append(names, old)
	}
	sort.Strings(names)
	return names
}

// таблица, по которой реально не итерируются, ничем не отличается от комментария:
// TestLoadConfigRenamedEnvVarNewNameStillApplies проходит по ней всей подтестами
var renamedEnvVarNewNameChecks = map[string]struct {
	value string
	get   func(Config) string
}{
	"GOTCHA_METRIC_EVAL_INTERVAL_SECONDS":  {"301", func(c Config) string { return strconv.Itoa(c.MetricEvalInterval) }},
	"GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS": {"302", func(c Config) string { return strconv.Itoa(c.ProfileEvalInterval) }},
	"GOTCHA_HOST_EVAL_INTERVAL_SECONDS":    {"303", func(c Config) string { return strconv.Itoa(c.HostEvalInterval) }},
	"GOTCHA_SLO_EVAL_INTERVAL_SECONDS":     {"304", func(c Config) string { return strconv.Itoa(c.SLOEvalInterval) }},
	"GOTCHA_ESCALATION_INTERVAL_SECONDS":   {"305", func(c Config) string { return strconv.Itoa(c.EscalationInterval) }},
	"GOTCHA_EVENT_RETENTION_DAYS":          {"306", func(c Config) string { return strconv.Itoa(c.RetentionDays) }},
	"GOTCHA_PROBE_SERVER_URL":              {"https://renamed-regression.example", func(c Config) string { return c.ServerURL }},
	"GOTCHA_INGEST_RATE_PER_SEC":           {"307", func(c Config) string { return strconv.Itoa(c.IngestRateLimit) }},
	"GOTCHA_DIST_DIR":                      {"/tmp/renamed-regression-dist", func(c Config) string { return c.AgentDistDir }},
	"GOTCHA_DIST_RATE_PER_MIN":             {"308", func(c Config) string { return strconv.Itoa(c.AgentDistRatePerMin) }},
	"GOTCHA_LISTEN_ADDR":                   {":9309", func(c Config) string { return c.Addr }},
	"GOTCHA_LOGGING_LEVEL":                 {"debug", func(c Config) string { return c.LogLevel }},
	"GOTCHA_LOGGING_FORMAT":                {"json", func(c Config) string { return c.LogFormat }},
	"GOTCHA_UPTIME_LOCAL_REGION":           {"renamed-regression-region", func(c Config) string { return c.LocalRegion }},
	"GOTCHA_REGISTRATION_MODE":             {"open", func(c Config) string { return c.RegistrationMode }},
	"GOTCHA_EXPORT_RETENTION_HOURS":        {"309", func(c Config) string { return strconv.Itoa(c.ExportTTLHours) }},
	"GOTCHA_SCRUB_DENY_KEYS": {"renamed_regression_deny_key", func(c Config) string {
		for _, k := range c.ScrubKeys {
			if k == "renamed_regression_deny_key" {
				return k
			}
		}
		return ""
	}},
	"GOTCHA_SCRUB_KEEP_KEYS": {"renamed_regression_keep_key", func(c Config) string {
		for _, k := range c.ScrubAllowKeys {
			if k == "renamed_regression_keep_key" {
				return k
			}
		}
		return ""
	}},
	"GOTCHA_EVALUATORS_ENABLED": {"true", func(c Config) string {
		if c.RunEvaluators == nil {
			return "nil"
		}
		return strconv.FormatBool(*c.RunEvaluators)
	}},
	"GOTCHA_AUTO_MIGRATE_ENABLED":             {"false", func(c Config) string { return strconv.FormatBool(c.AutoMigrate) }},
	"GOTCHA_SECRET_KEY_ALLOW_INSECURE":        {"true", func(c Config) string { return strconv.FormatBool(c.AllowInsecureSecret) }},
	"GOTCHA_MAX_WRITER_BUFFER_BYTES":          {"26214400", func(c Config) string { return strconv.FormatInt(c.MaxBufferBytes, 10) }},
	"GOTCHA_MAX_INGEST_QUEUE_BYTES":           {"10485760", func(c Config) string { return strconv.FormatInt(c.MaxQueueBytes, 10) }},
	"GOTCHA_PROBE_KEY":                        {"renamed-regression-probe-key", func(c Config) string { return c.ProbeToken }},
	"GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED": {"true", func(c Config) string { return strconv.FormatBool(c.ExternalChannelDetails) }},
	"GOTCHA_OIDC_DISPLAY_NAME":                {"Renamed Regression SSO", func(c Config) string { return c.OIDCName }},
	"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS":    {"309", func(c Config) string { return strconv.Itoa(c.PurgeReconcileHours) }},
}

// подтест на каждую пару реестра, не одна проверка на первой паре: таблица
// без реального обхода защищает одну строку из многих, а не все
func TestLoadConfigRenamedEnvVarFailsStart(t *testing.T) {
	// sortedRenamedOldNames(), вручную урезанная, по-прежнему вернула бы валидный
	// []string — сверка длины с envcontract.Renamed напрямую ловит и эту мутацию
	if got, want := len(sortedRenamedOldNames()), len(envcontract.Renamed); got != want {
		t.Fatalf("sortedRenamedOldNames() вернула %d имён, envcontract.Renamed содержит %d — обход урезан, ниже проверится не весь реестр", got, want)
	}
	for _, old := range sortedRenamedOldNames() {
		newName := envcontract.Renamed[old]
		t.Run(old, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{old: "some-value"}), nil)
			if err == nil {
				t.Fatalf("loadConfig: want ошибку на устаревшем %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) {
				t.Errorf("err = %q, want упоминание старого имени %s", err, old)
			}
			if !strings.Contains(err.Error(), newName) {
				t.Errorf("err = %q, want упоминание нового имени %s", err, newName)
			}
		})
	}
}

func TestLoadConfigRenamedEnvVarsListsAllFindings(t *testing.T) {
	names := sortedRenamedOldNames()
	old1, old2 := names[0], names[1]

	_, err := loadConfig(getenvFrom(map[string]string{
		old1: "x",
		old2: "y",
	}), nil)
	if err == nil {
		t.Fatalf("loadConfig: want ошибку на двух устаревших именах, получили nil")
	}
	for _, old := range []string{old1, old2} {
		if !strings.Contains(err.Error(), old) {
			t.Errorf("err = %q, want упоминание %s (найдено больше одного устаревшего имени)", err, old)
		}
	}
}

// зовёт loadConfigChecked, а не голый loadConfig: последний по-прежнему пропускает
// пустое значение (это забота только CheckRenamedAll), но прод идёт через checked
func TestLoadConfigRenamedEnvVarEmptyNowFailsStart(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	newName := envcontract.Renamed[old]
	_, err := loadConfigChecked(getenvFrom(map[string]string{old: ""}), environFrom(old+"="), nil)
	if err == nil {
		t.Fatalf("loadConfigChecked с пустым устаревшим %s: want ошибку (declared-but-unset больше не спасает переименованное имя), получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
	if strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %q, want текст переименования, а не «unknown … typos» — это не опечатка, а известное старое имя", err)
	}
}

func TestLoadConfigBareStillIgnoresEmptyRenamedName(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	if _, err := loadConfig(getenvFrom(map[string]string{old: ""}), nil); err != nil {
		t.Errorf("loadConfig с пустым устаревшим %s: %v, want nil (собственный контракт loadConfig не изменился)", old, err)
	}
}

// подтест на каждую запись таблицы: иначе она защищала бы одно переименование,
// а остальные строки были бы никогда не вызываемым мёртвым кодом
func TestLoadConfigRenamedEnvVarNewNameStillApplies(t *testing.T) {
	newNames := make([]string, 0, len(renamedEnvVarNewNameChecks))
	for newName := range renamedEnvVarNewNameChecks {
		newNames = append(newNames, newName)
	}
	sort.Strings(newNames)

	for _, newName := range newNames {
		check := renamedEnvVarNewNameChecks[newName]
		t.Run(newName, func(t *testing.T) {
			cfg, err := loadConfig(getenvFrom(map[string]string{newName: check.value}), nil)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := check.get(cfg); got != check.value {
				t.Errorf("%s=%q: соответствующее поле Config = %q, want %q (регрессия применения нового имени)", newName, check.value, got, check.value)
			}
		})
	}
}

func TestLoadConfig_HSTSDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.HSTSEnabled {
		t.Error("HSTSEnabled default = false, want true")
	}
	if cfg.HSTSMaxAgeSeconds != 31536000 {
		t.Errorf("HSTSMaxAgeSeconds default = %d, want 31536000", cfg.HSTSMaxAgeSeconds)
	}
	if cfg.HSTSIncludeSubDomains {
		t.Error("HSTSIncludeSubDomains default = true, want false")
	}
	if cfg.HSTSPreload {
		t.Error("HSTSPreload default = true, want false")
	}
}

func TestLoadConfig_HSTSOverrides(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_MAX_AGE_SECONDS":    "600",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HSTSMaxAgeSeconds != 600 || !cfg.HSTSIncludeSubDomains {
		t.Errorf("HSTS = (%d, %v), want (600, true)", cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains)
	}
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_HSTS_MAX_AGE_SECONDS": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig max-age=0: %v", err)
	}
	if cfg.HSTSMaxAgeSeconds != 0 {
		t.Errorf("HSTSMaxAgeSeconds = %d, want 0", cfg.HSTSMaxAgeSeconds)
	}
}

func TestLoadConfig_HSTSRejects(t *testing.T) {
	for _, tc := range []struct {
		name          string
		env           map[string]string
		wantErrSubstr string
	}{
		{"negative max-age", map[string]string{"GOTCHA_HSTS_MAX_AGE_SECONDS": "-1"},
			"GOTCHA_HSTS_MAX_AGE_SECONDS must be >= 0"},
		{"preload without subdomains", map[string]string{"GOTCHA_HSTS_PRELOAD": "true"},
			"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_INCLUDE_SUBDOMAINS=true"},
		{"preload with short max-age", map[string]string{
			"GOTCHA_HSTS_PRELOAD":            "true",
			"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
			"GOTCHA_HSTS_MAX_AGE_SECONDS":    "600",
		}, "GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_MAX_AGE_SECONDS >= 31536000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(tc.env), nil)
			if err == nil {
				t.Fatal("конфиг принят, ожидался отказ старта")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("ошибка = %q, want подстроку %q (сработало не то правило)", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// без позитивного кейса на границе (31536000) замена `<` на `<=` в валидации
// не роняет ни один тест
func TestLoadConfig_HSTSPreloadAccepted(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_ENABLED":              "true",
		"GOTCHA_HSTS_PRELOAD":              "true",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS":   "true",
		"GOTCHA_HSTS_MAX_AGE_SECONDS":      "31536000",
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}), nil)
	if err != nil {
		t.Fatalf("корректная preload-конфигурация на границе (max-age=31536000) обязана стартовать: %v", err)
	}
	got := web.HSTSHeaderValue(cfg.HSTSEnabled, cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains, cfg.HSTSPreload)
	want := "max-age=31536000; includeSubDomains; preload"
	if got != want {
		t.Errorf("собранный заголовок = %q, want %q", got, want)
	}
}

func TestLoadConfig_HSTSDisabledSkipsPreloadChecks(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_ENABLED":         "false",
		"GOTCHA_HSTS_PRELOAD":         "true",
		"GOTCHA_HSTS_MAX_AGE_SECONDS": "600",
	}), nil)
	if err != nil {
		t.Fatalf("выключенный HSTS с preload обязан стартовать: %v", err)
	}
	if cfg.HSTSEnabled {
		t.Error("HSTSEnabled = true, want false")
	}
}

func TestLoadConfig_HSTSWarnings(t *testing.T) {
	capture := func(t *testing.T, env map[string]string) []slog.Record {
		t.Helper()
		var records []slog.Record
		prev := slog.Default()
		slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
		defer slog.SetDefault(prev)
		if _, err := loadConfig(getenvFrom(env), nil); err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		return records
	}
	hasWarn := func(records []slog.Record, substr string) bool {
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
				return true
			}
		}
		return false
	}

	if got := capture(t, map[string]string{
		"GOTCHA_HSTS_ENABLED":              "true",
		"GOTCHA_BASE_URL":                  "http://gotcha.example",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}); !hasWarn(got, "GOTCHA_BASE_URL") {
		t.Error("нет предупреждения о том, что HSTS включён при не-https GOTCHA_BASE_URL")
	}
	if got := capture(t, map[string]string{"GOTCHA_HSTS_ENABLED": "true"}); hasWarn(got, "GOTCHA_BASE_URL") {
		t.Error("предупреждение выдано на дефолтном GOTCHA_BASE_URL=http://localhost:8080 (штатный квикстарт)")
	}
	if got := capture(t, map[string]string{
		"GOTCHA_HSTS_ENABLED":            "false",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
	}); !hasWarn(got, "GOTCHA_HSTS_INCLUDE_SUBDOMAINS") {
		t.Error("нет предупреждения о том, что настройка HSTS игнорируется при выключенном HSTS")
	}
	if got := capture(t, map[string]string{
		"GOTCHA_HSTS_ENABLED":              "false",
		"GOTCHA_BASE_URL":                  "https://gotcha.example",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}); hasWarn(got, "ignored") {
		t.Error("предупреждение об игнорировании выдано, хотя ни одна настройка HSTS не задана")
	}
}

// в ingest/uptime/probe web.Handler не строится, заголовок структурно невозможен —
// предупреждение про не-https BaseURL там не о чем показывать
func TestLoadConfig_HSTSWarningSkippedOutsideWebModes(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prev)

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_HSTS_ENABLED": "true"}),
		[]string{"--mode", "ingest"}); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "GOTCHA_BASE_URL") {
			t.Errorf("предупреждение про GOTCHA_BASE_URL выдано в режиме ingest, где web.Handler не строится: %q", r.Message)
		}
	}
}

func TestLoadConfig_StringEnvTrimmed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_LOCALE":              " ru",
		"GOTCHA_UPTIME_LOCAL_REGION": " eu-fra \t",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Locale != "ru" {
		t.Errorf("Locale = %q, want %q (leading space must be trimmed)", cfg.Locale, "ru")
	}
	if cfg.LocalRegion != "eu-fra" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "eu-fra")
	}
}

func TestLoadConfig_WhitespaceOnlyStringFallsBackToDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_UPTIME_LOCAL_REGION": "   "}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.LocalRegion != "local" {
		t.Errorf("LocalRegion = %q, want default %q (whitespace-only counts as unset)", cfg.LocalRegion, "local")
	}
}

// BaseURL по умолчанию localhost: проверка силы ключа тут не участвует
func TestLoadConfig_SecretKeyTrimmed(t *testing.T) {
	for _, raw := range []string{"abc", "abc ", " abc", "\tabc\n"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_SECRET_KEY": raw}), nil)
		if err != nil {
			t.Fatalf("GOTCHA_SECRET_KEY=%q: loadConfig: %v", raw, err)
		}
		if cfg.SecretKey != "abc" {
			t.Errorf("GOTCHA_SECRET_KEY=%q: SecretKey = %q, want %q", raw, cfg.SecretKey, "abc")
		}
	}
}

func TestLoadConfig_BlankGuardedStringsRejected(t *testing.T) {
	for _, key := range []string{"GOTCHA_SECRET_KEY", "GOTCHA_PG_DSN", "GOTCHA_CH_DSN"} {
		_, err := loadConfig(getenvFrom(map[string]string{key: "   "}), nil)
		if err == nil {
			t.Errorf("%s=\"   \": ждали ошибку старта, получили nil", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s=\"   \": ошибка не называет переменную: %v", key, err)
		}
	}
}

func TestLoadConfig_GuardedDSNsTrimmed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_PG_DSN": " postgres://u:p@pg:5432/g ",
		"GOTCHA_CH_DSN": " clickhouse://ch:9000/g\t",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.PostgresDSN != "postgres://u:p@pg:5432/g" {
		t.Errorf("PostgresDSN = %q", cfg.PostgresDSN)
	}
	if cfg.ClickHouseDSN != "clickhouse://ch:9000/g" {
		t.Errorf("ClickHouseDSN = %q", cfg.ClickHouseDSN)
	}
}

// читаются обычным str(), пробельное значение уже трактуется как «не задано»
func TestLoadConfig_ProbeCredsWhitespaceOnlyRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"пробельный токен": {
			"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
			"GOTCHA_PROBE_KEY":        "   ",
		},
		"пробельный server url": {
			"GOTCHA_PROBE_SERVER_URL": "   ",
			"GOTCHA_PROBE_KEY":        "ptok",
		},
	}
	for name, env := range cases {
		if _, err := loadConfig(getenvFrom(env), []string{"--mode=probe"}); err == nil {
			t.Errorf("%s: ждали отказ старта в --mode=probe, получили nil", name)
		}
	}
}

func TestLoadConfigExportValidatedAtStartup(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_EXPORT_MAX_ROWS", "0"},
		{"GOTCHA_EXPORT_MAX_BYTES", "0"},
		{"GOTCHA_EXPORT_DISK_BUDGET_BYTES", "0"},
		{"GOTCHA_EXPORT_RETENTION_HOURS", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
			for _, field := range []string{"MaxRows", "MaxBytes", "DiskBudget"} {
				if strings.Contains(err.Error(), field) {
					t.Errorf("%s=%s: error %q still names the Go struct field %q instead of the env var", tc.key, tc.value, err, field)
				}
			}
		})
	}

	// цикл выше гонял дефолтный режим ("all"); именно --mode=ingest поднимает export-воркер
	t.Run("mode=ingest", func(t *testing.T) {
		_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EXPORT_MAX_ROWS": "0"}), []string{"--mode=ingest"})
		if err == nil {
			t.Fatal("--mode=ingest, GOTCHA_EXPORT_MAX_ROWS=0: want error, got nil")
		}
		if !strings.Contains(err.Error(), "GOTCHA_EXPORT_MAX_ROWS") {
			t.Errorf("--mode=ingest: error %q does not name the variable", err)
		}
	})
}

func TestLoadConfig_ValidationErrorsReportedTogether(t *testing.T) {
	env := map[string]string{
		"GOTCHA_REGISTRATION_MODE":    "weird",
		"GOTCHA_LOCALE":               "de",
		"GOTCHA_EVENT_RETENTION_DAYS": "-1",
		"GOTCHA_SMTP_PORT":            "70000",
		"GOTCHA_OIDC_ENABLED":         "1",
		"GOTCHA_LOGGING_LEVEL":        "trace",
		"GOTCHA_LOGGING_FORMAT":       "xml",
	}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	want := []string{
		"GOTCHA_REGISTRATION_MODE",
		"GOTCHA_LOCALE",
		"GOTCHA_EVENT_RETENTION_DAYS",
		"GOTCHA_SMTP_PORT",
		"GOTCHA_OIDC_ENABLED",
		"GOTCHA_LOGGING_LEVEL",
		"GOTCHA_LOGGING_FORMAT",
	}
	for _, name := range want {
		if !strings.Contains(msg, name) {
			t.Errorf("error text is missing %s; full text:\n%s", name, msg)
		}
	}
}

func TestTranslateExportEnvNamesWholeWordOnly(t *testing.T) {
	msg := "export: конфигурация: JobTimeout (15m0s) обязан быть строго меньше leaseTTL (20m0s)"
	got := translateExportEnvNames(msg)
	if got != msg {
		t.Errorf("translateExportEnvNames(%q) = %q, want unchanged (leaseTTL — не поле export.Config, а внутренний пакетный термин)", msg, got)
	}
	if strings.Contains(got, "GOTCHA_EXPORT_RETENTION_HOURS") {
		t.Errorf("translateExportEnvNames(%q) = %q, leaseTTL изуродован подстрочной заменой", msg, got)
	}

	real := "export: конфигурация: TTL (0s) обязан быть положительным"
	want := "export: конфигурация: GOTCHA_EXPORT_RETENTION_HOURS (0s) обязан быть положительным"
	if got := translateExportEnvNames(real); got != want {
		t.Errorf("translateExportEnvNames(%q) = %q, want %q", real, got, want)
	}
}

func TestLoadConfigAllowInsecureSecretGarbageParsedRegardlessOfKeyStrength(t *testing.T) {
	strong := strings.Repeat("a", 32) // ровно 32 байта — сильный ключ
	short := "0123456789abcdef"       // 16 байт — слабый ключ

	t.Run("нормальный ключ + мусор", func(t *testing.T) {
		_, err := loadConfig(getenvFrom(map[string]string{
			"GOTCHA_BASE_URL":                  "https://gotcha.example",
			"GOTCHA_SECRET_KEY":                strong,
			"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "ture",
		}), []string{"--mode=web"})
		if err == nil {
			t.Fatal("сильный ключ + мусор в GOTCHA_SECRET_KEY_ALLOW_INSECURE: want error, got nil (раньше значение не разбиралось вовсе)")
		}
		if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_ALLOW_INSECURE") || !strings.Contains(err.Error(), "invalid boolean") {
			t.Errorf("error = %q, want it to name GOTCHA_SECRET_KEY_ALLOW_INSECURE and say 'invalid boolean'", err)
		}
	})

	t.Run("слабый ключ + мусор", func(t *testing.T) {
		_, err := loadConfig(getenvFrom(map[string]string{
			"GOTCHA_BASE_URL":                  "https://gotcha.example",
			"GOTCHA_SECRET_KEY":                short,
			"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "ture",
		}), []string{"--mode=web"})
		if err == nil {
			t.Fatal("слабый ключ + мусор в GOTCHA_SECRET_KEY_ALLOW_INSECURE: want error, got nil")
		}
	})
}

func TestLoadConfig_EnumsCaseInsensitive(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION":           "OSS",
		"GOTCHA_REGISTRATION_MODE": "OPEN",
		"GOTCHA_LOCALE":            "EN",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Edition != "oss" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "oss")
	}
	if cfg.RegistrationMode != "open" {
		t.Errorf("RegistrationMode = %q, want %q", cfg.RegistrationMode, "open")
	}
	if cfg.Locale != "en" {
		t.Errorf("Locale = %q, want %q", cfg.Locale, "en")
	}

	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION": " SaaS ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig mixed case: %v", err)
	}
	if cfg.Edition != "saas" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "saas")
	}
	if cfg.DefaultEventQuota != 1_000_000 {
		t.Errorf("DefaultEventQuota (SaaS с пробелами) = %d, want 1000000 — редакция должна распознаться", cfg.DefaultEventQuota)
	}

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "BOGUS"}), nil); err == nil {
		t.Error("BOGUS GOTCHA_EDITION must fail regardless of case")
	}
}

// loadConfig сам не валидирует уровень/формат — это делает setupLogging в main.go
func TestLoadConfig_LogLevelFormatTrimmedAndLowered(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_LOGGING_LEVEL":  " WARNING ",
		"GOTCHA_LOGGING_FORMAT": " JSON ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.LogLevel != "warning" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "warning")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
}

func TestTrustedRecipientsWhitespaceAndEmptyElements(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_TRUSTED_RECIPIENTS": " a.example , ,b.example ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"a.example", "b.example"}
	if len(cfg.TrustedRecipients) != len(want) {
		t.Fatalf("TrustedRecipients = %v, want %v", cfg.TrustedRecipients, want)
	}
	for i, w := range want {
		if cfg.TrustedRecipients[i] != w {
			t.Errorf("TrustedRecipients[%d] = %q, want %q", i, cfg.TrustedRecipients[i], w)
		}
	}
}

func TestLoadConfig_SMTPRequireTLSDefaultsFalse(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SMTPRequireTLS {
		t.Error("SMTPRequireTLS default = true, want false")
	}
}

func TestLoadConfig_SMTPRequireTLSOverride(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_SMTP_REQUIRE_TLS": "true"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SMTPRequireTLS {
		t.Error("SMTPRequireTLS = false, want true")
	}
}
