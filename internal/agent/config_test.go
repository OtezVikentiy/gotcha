package agent

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Go передаёт результат многозначного вызова как оба аргумента, если это
// единственный аргумент — отсюда LoadConfig(env(vars)) без распаковки.
func env(m map[string]string) (func(string) string, func() []string) {
	getenv := func(k string) string { return m[k] }
	environ := func() []string {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, k+"="+v)
		}
		return out
	}
	return getenv, environ
}

func environFrom(kv ...string) func() []string {
	return func() []string { return kv }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk_x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("interval default = %v", cfg.Interval)
	}
	if cfg.InsecureSkipVerify {
		t.Error("skip verify должен быть false по умолчанию")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]map[string]string{
		"без endpoint":                    {"GOTCHA_AGENT_INGEST_KEY": "pk"},
		"без key":                         {"GOTCHA_AGENT_ENDPOINT": "https://g"},
		"кривой endpoint":                 {"GOTCHA_AGENT_ENDPOINT": "ftp://g", "GOTCHA_AGENT_INGEST_KEY": "pk"},
		"interval мал":                    {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "5"},
		"interval велик":                  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "301"},
		"interval мусор":                  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "щедро"},
		"interval старый duration-формат": {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "30s"},
	}
	for name, vars := range cases {
		if _, err := LoadConfig(env(vars)); err == nil {
			t.Errorf("%s: ждали ошибку", name)
		}
	}
}

func TestLoadConfigLabels(t *testing.T) {
	vars := map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://x.example", "GOTCHA_AGENT_INGEST_KEY": "k",
		"GOTCHA_AGENT_ENVIRONMENT": "prod", "GOTCHA_AGENT_ROLE": "web",
	}
	cfg, err := LoadConfig(env(vars))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Environment != "prod" || cfg.Role != "web" {
		t.Fatalf("labels=(%q,%q), want (prod,web)", cfg.Environment, cfg.Role)
	}
}

func TestLoadConfigTLSSkipVerifyAcceptedSpellings(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"YES", true}, {"on", true}, {" on ", true},
		{"0", false}, {"false", false}, {"FALSE", false}, {"no", false}, {"NO", false}, {"off", false}, {" off ", false},
	} {
		vars := map[string]string{
			"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
			"GOTCHA_AGENT_INGEST_KEY": "pk",
		}
		if tc.value != "" {
			vars["GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY"] = tc.value
		}
		cfg, err := LoadConfig(env(vars))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY=%q: LoadConfig: %v", tc.value, err)
		}
		if cfg.InsecureSkipVerify != tc.want {
			t.Errorf("GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY=%q: InsecureSkipVerify = %v, want %v", tc.value, cfg.InsecureSkipVerify, tc.want)
		}
	}
}

func TestLoadConfigTLSSkipVerifyRejectsInvalid(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":                 "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY":               "pk",
		"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": "ture",
	}))
	if err == nil {
		t.Fatal("GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY=ture: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY and say 'invalid boolean'", err)
	}
}

func TestLoadConfigTrimsEndpointSlash(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example/",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://g.example" {
		t.Errorf("endpoint = %q — хвостовой / не срезан (иначе //v1/metrics)", cfg.Endpoint)
	}
}

func TestLoadConfigTrimsEndpointLeadingAndTrailingSpace(t *testing.T) {
	cases := []string{
		" https://g.example",
		"https://g.example ",
		"https://g.example/ ",
		"\thttps://g.example\n",
	}
	for _, raw := range cases {
		cfg, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT":   raw,
			"GOTCHA_AGENT_INGEST_KEY": "pk",
		}))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_ENDPOINT=%q: LoadConfig: %v", raw, err)
		}
		if cfg.Endpoint != "https://g.example" {
			t.Errorf("GOTCHA_AGENT_ENDPOINT=%q: Endpoint = %q, want %q", raw, cfg.Endpoint, "https://g.example")
		}
	}
}

func TestLoadConfigEndpointRejectsQuery(t *testing.T) {
	for _, raw := range []string{
		"https://g.example?token=1",
		"https://g.example#frag",
	} {
		if _, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT":   raw,
			"GOTCHA_AGENT_INGEST_KEY": "pk",
		})); err == nil {
			t.Errorf("GOTCHA_AGENT_ENDPOINT=%q: want error, got nil", raw)
		}
	}
}

func TestLoadConfigEndpointNormalizeErrorPassedThroughVerbatim(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example?token=1",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
	}))
	if err == nil {
		t.Fatal("GOTCHA_AGENT_ENDPOINT с query: want error, got nil")
	}
	if !strings.Contains(err.Error(), "must not carry a query or fragment") {
		t.Errorf("error = %q, want точный текст baseurl.Normalize (must not carry a query or fragment), а не общую формулировку", err)
	}
	if strings.Contains(err.Error(), "must be an http(s) URL") {
		t.Errorf("error = %q, want НЕ общую формулировку — она называет неверную причину для адреса, у которого схема и хост есть", err)
	}
}

func TestLoadConfigWhitespaceOnlyEndpointRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "   ",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_ENDPOINT должен ронять старт")
	}
}

func TestLoadConfigWhitespaceOnlyKeyRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "   ",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_INGEST_KEY должен ронять старт")
	}
}

func TestLoadConfigTrimsKeyCACertLabels(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":    "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY":  "abc ",
		"GOTCHA_AGENT_CA_CERT":     " /etc/gotcha/ca.pem\t",
		"GOTCHA_AGENT_ENVIRONMENT": " prod ",
		"GOTCHA_AGENT_ROLE":        " web\n",
		"GOTCHA_AGENT_HOSTNAME":    " web-1 ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Key != "abc" {
		t.Errorf("Key = %q, want %q", cfg.Key, "abc")
	}
	if cfg.CACert != "/etc/gotcha/ca.pem" {
		t.Errorf("CACert = %q, want %q", cfg.CACert, "/etc/gotcha/ca.pem")
	}
	if cfg.Environment != "prod" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "prod")
	}
	if cfg.Role != "web" {
		t.Errorf("Role = %q, want %q", cfg.Role, "web")
	}
	if cfg.Hostname != "web-1" {
		t.Errorf("Hostname = %q, want %q", cfg.Hostname, "web-1")
	}
}

func TestLoadConfigHostnameTrimmedConsistently(t *testing.T) {
	for _, raw := range []string{"web-1", "web-1 ", " web-1", "\tweb-1\n"} {
		cfg, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
			"GOTCHA_AGENT_INGEST_KEY": "pk",
			"GOTCHA_AGENT_HOSTNAME":   raw,
		}))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_HOSTNAME=%q: LoadConfig: %v", raw, err)
		}
		if cfg.Hostname != "web-1" {
			t.Errorf("GOTCHA_AGENT_HOSTNAME=%q: Hostname = %q, want %q", raw, cfg.Hostname, "web-1")
		}
	}
}

// httptest.Server увидел бы уже нормализованное значение (net/http/textproto
// обрезает OWS сам) — round-trip через него маскировал бы регресс тримминга.
func TestLoadConfigKeyTrimReflectedInBearerHeader(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "abc ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.Endpoint+"/v1/metrics", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	if got := req.Header.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer abc")
	}
}

func TestLoadConfigIntervalSecondsApplied(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":         "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY":       "pk",
		"GOTCHA_AGENT_INTERVAL_SECONDS": "30",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", cfg.Interval)
	}
}

func TestLoadConfigIntervalSecondsBoundaryError(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":         "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY":       "pk",
		"GOTCHA_AGENT_INTERVAL_SECONDS": "5",
	}))
	if err == nil {
		t.Fatal("GOTCHA_AGENT_INTERVAL_SECONDS=5: want error, got nil")
	}
	if !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "300") {
		t.Errorf("error = %q, want it to name the boundaries 10..300", err)
	}
}

func sortedAgentOwnedOldNames() []string {
	names := make([]string, len(envcontract.AgentOwned))
	copy(names, envcontract.AgentOwned)
	sort.Strings(names)
	return names
}

// Находится динамически, не литералом: internal/guards/renamed_env_vars_test.go
// не пускает старые имена литералом за пределы renamed.go/CHANGELOG/upgrade.md.
func foreignRenamedNameUnderOwnPrefix(t *testing.T) string {
	t.Helper()
	agentOwned := map[string]bool{}
	for _, old := range envcontract.AgentOwned {
		agentOwned[old] = true
	}
	var candidates []string
	for old := range envcontract.Renamed {
		if !agentOwned[old] && strings.HasPrefix(old, "GOTCHA_AGENT_") {
			candidates = append(candidates, old)
		}
	}
	if len(candidates) == 0 {
		t.Fatal("обход ослеп: в envcontract.Renamed не нашлось имени вне AgentOwned, но под префиксом GOTCHA_AGENT_")
	}
	sort.Strings(candidates)
	return candidates[0]
}

func TestLoadConfigRenamedEnvVarFailsStart(t *testing.T) {
	for _, old := range sortedAgentOwnedOldNames() {
		newName := envcontract.Renamed[old]
		t.Run(old, func(t *testing.T) {
			_, err := LoadConfig(env(map[string]string{old: "some-value"}))
			if err == nil {
				t.Fatalf("LoadConfig: want ошибку на устаревшем %s, получили nil", old)
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

// checkUnknownAgentEnvVars смотрит на имя, не на значение — declared-but-unset
// не спасает переименованное имя, которое уже никто не читает.
func TestLoadConfigRenamedEnvVarEmptyNowFailsStart(t *testing.T) {
	old := sortedAgentOwnedOldNames()[0]
	newName := envcontract.Renamed[old]
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
		old:                       "",
	}))
	if err == nil {
		t.Fatalf("LoadConfig с пустым устаревшим %s: want ошибку (declared-but-unset больше не спасает переименованное имя), получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
}

func TestLoadConfigIgnoresOutOfScopeRenamedNames(t *testing.T) {
	agentOwned := map[string]bool{}
	for _, old := range envcontract.AgentOwned {
		agentOwned[old] = true
	}
	outOfScope := ""
	for old := range envcontract.Renamed {
		// Без проверки префикса обход map мог бы недетерминированно выбрать
		// серверное имя, которое агент обязан назвать renamed (см. тест ниже).
		if !agentOwned[old] && !strings.HasPrefix(old, "GOTCHA_AGENT_") {
			outOfScope = old
			break
		}
	}
	if outOfScope == "" {
		t.Fatal("обход ослеп: в envcontract.Renamed не нашлось имени вне AgentOwned и вне префикса GOTCHA_AGENT_")
	}
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
		outOfScope:                "some-value",
	}))
	if err != nil {
		t.Fatalf("LoadConfig с посторонним устаревшим %s: %v, want nil (не своя переменная)", outOfScope, err)
	}
	if cfg.Endpoint != "https://g.example" {
		t.Errorf("Endpoint = %q, want https://g.example", cfg.Endpoint)
	}
}

func TestLoadConfigRejectsForeignRenamedNameUnderOwnPrefix(t *testing.T) {
	old := foreignRenamedNameUnderOwnPrefix(t)
	newName := envcontract.Renamed[old]
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
		old:                       "/opt/x",
	}))
	if err == nil {
		t.Fatalf("LoadConfig с %s=/opt/x: want ошибку renamed, получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
	if strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %q, want текст переименования, а не «unknown … typos» — это не опечатка, а известное старое имя", err)
	}
}

var agentRenamedEnvVarNewNameChecks = map[string]struct {
	value string
	get   func(Config) string
}{
	"GOTCHA_AGENT_INTERVAL_SECONDS":         {"120", func(c Config) string { return strconv.Itoa(int(c.Interval.Seconds())) }},
	"GOTCHA_AGENT_INGEST_KEY":               {"renamed-regression-key", func(c Config) string { return c.Key }},
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": {"true", func(c Config) string { return strconv.FormatBool(c.InsecureSkipVerify) }},
}

func TestAgentRenamedEnvVarNewNameChecksComplete(t *testing.T) {
	wantNewNames := map[string]bool{}
	for _, old := range envcontract.AgentOwned {
		wantNewNames[envcontract.Renamed[old]] = true
	}
	for newName := range agentRenamedEnvVarNewNameChecks {
		if !wantNewNames[newName] {
			t.Errorf("agentRenamedEnvVarNewNameChecks содержит лишнюю запись %s", newName)
		}
	}
	for newName := range wantNewNames {
		if _, ok := agentRenamedEnvVarNewNameChecks[newName]; !ok {
			t.Errorf("agentRenamedEnvVarNewNameChecks не хватает записи для %s", newName)
		}
	}
}

func TestLoadConfigRenamedEnvVarNewNameStillApplies(t *testing.T) {
	for newName, check := range agentRenamedEnvVarNewNameChecks {
		t.Run(newName, func(t *testing.T) {
			vars := map[string]string{"GOTCHA_AGENT_ENDPOINT": "https://g.example"}
			if newName != "GOTCHA_AGENT_INGEST_KEY" {
				vars["GOTCHA_AGENT_INGEST_KEY"] = "pk"
			}
			vars[newName] = check.value
			cfg, err := LoadConfig(env(vars))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got := check.get(cfg); got != check.value {
				t.Errorf("%s=%q: read back %q, want %q", newName, check.value, got, check.value)
			}
		})
	}
}
