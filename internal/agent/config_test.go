package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://g.example",
		"GOTCHA_AGENT_KEY":      "pk_x",
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
		"без endpoint":    {"GOTCHA_AGENT_KEY": "pk"},
		"без key":         {"GOTCHA_AGENT_ENDPOINT": "https://g"},
		"кривой endpoint": {"GOTCHA_AGENT_ENDPOINT": "ftp://g", "GOTCHA_AGENT_KEY": "pk"},
		"interval мал":    {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_KEY": "pk", "GOTCHA_AGENT_INTERVAL": "5s"},
		"interval велик":  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_KEY": "pk", "GOTCHA_AGENT_INTERVAL": "10m"},
		"interval мусор":  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_KEY": "pk", "GOTCHA_AGENT_INTERVAL": "щедро"},
	}
	for name, vars := range cases {
		if _, err := LoadConfig(env(vars)); err == nil {
			t.Errorf("%s: ждали ошибку", name)
		}
	}
}

func TestLoadConfigLabels(t *testing.T) {
	env := map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://x.example", "GOTCHA_AGENT_KEY": "k",
		"GOTCHA_AGENT_ENVIRONMENT": "prod", "GOTCHA_AGENT_ROLE": "web",
	}
	cfg, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Environment != "prod" || cfg.Role != "web" {
		t.Fatalf("labels=(%q,%q), want (prod,web)", cfg.Environment, cfg.Role)
	}
}

// TestLoadConfigTLSSkipVerifyAcceptedSpellings — GOTCHA_AGENT_TLS_SKIP_VERIFY
// принимает ровно тот же набор написаний, что общий разбор булевых в
// cmd/gotcha (trim + lower; 1/true/yes/on и 0/false/no/off в обоих
// регистрах). Раньше switch был буквальным и падал на "True".
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
			"GOTCHA_AGENT_ENDPOINT": "https://g.example",
			"GOTCHA_AGENT_KEY":      "pk",
		}
		if tc.value != "" {
			vars["GOTCHA_AGENT_TLS_SKIP_VERIFY"] = tc.value
		}
		cfg, err := LoadConfig(env(vars))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_TLS_SKIP_VERIFY=%q: LoadConfig: %v", tc.value, err)
		}
		if cfg.InsecureSkipVerify != tc.want {
			t.Errorf("GOTCHA_AGENT_TLS_SKIP_VERIFY=%q: InsecureSkipVerify = %v, want %v", tc.value, cfg.InsecureSkipVerify, tc.want)
		}
	}
}

// TestLoadConfigTLSSkipVerifyRejectsInvalid — мусор в булевой переменной не
// должен молча превращаться в false: см. TestLoadConfigRunEvaluatorsRejectsInvalid
// в cmd/gotcha за тем же контрактом.
func TestLoadConfigTLSSkipVerifyRejectsInvalid(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":        "https://g.example",
		"GOTCHA_AGENT_KEY":             "pk",
		"GOTCHA_AGENT_TLS_SKIP_VERIFY": "ture",
	}))
	if err == nil {
		t.Fatal("GOTCHA_AGENT_TLS_SKIP_VERIFY=ture: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_TLS_SKIP_VERIFY") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_AGENT_TLS_SKIP_VERIFY and say 'invalid boolean'", err)
	}
}

func TestLoadConfigTrimsEndpointSlash(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://g.example/",
		"GOTCHA_AGENT_KEY":      "pk",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://g.example" {
		t.Errorf("endpoint = %q — хвостовой / не срезан (иначе //v1/metrics)", cfg.Endpoint)
	}
}

// TestLoadConfigTrimsEndpointLeadingAndTrailingSpace — эндпоинт раньше
// триммился только по хвостовой "/"; пробел по краям ("https://g.example/ ",
// " https://g.example") проходил как есть. Пробел ПОСЛЕ хвостовой "/" вообще
// не срезался бы TrimRight(v, "/") — обрезка пробела должна идти ДО него.
func TestLoadConfigTrimsEndpointLeadingAndTrailingSpace(t *testing.T) {
	cases := []string{
		" https://g.example",
		"https://g.example ",
		"https://g.example/ ",
		"\thttps://g.example\n",
	}
	for _, raw := range cases {
		cfg, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT": raw,
			"GOTCHA_AGENT_KEY":      "pk",
		}))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_ENDPOINT=%q: LoadConfig: %v", raw, err)
		}
		if cfg.Endpoint != "https://g.example" {
			t.Errorf("GOTCHA_AGENT_ENDPOINT=%q: Endpoint = %q, want %q", raw, cfg.Endpoint, "https://g.example")
		}
	}
}

// TestLoadConfigEndpointRejectsQuery — E3 T6: GOTCHA_AGENT_ENDPOINT срезал
// хвостовой слэш и до этой правки, но query/fragment не проверял вовсе —
// baseurl.Normalize (тот же хелпер, что у GOTCHA_BASE_URL/
// GOTCHA_TELEGRAM_API_BASE/GOTCHA_PROBE_SERVER_URL в cmd/gotcha/config.go)
// закрывает и эту ветку: невалидное значение — отказ старта, а не адрес с
// query/fragment посреди пути в каждом OTLP-пуше.
func TestLoadConfigEndpointRejectsQuery(t *testing.T) {
	for _, raw := range []string{
		"https://g.example?token=1",
		"https://g.example#frag",
	} {
		if _, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT": raw,
			"GOTCHA_AGENT_KEY":      "pk",
		})); err == nil {
			t.Errorf("GOTCHA_AGENT_ENDPOINT=%q: want error, got nil", raw)
		}
	}
}

// TestLoadConfigWhitespaceOnlyEndpointRejected — эндпоинт обязателен; строка
// из одних пробелов после тримминга становится пустой и обязана давать ту же
// ошибку, что полностью отсутствующая переменная, а не URL с пробелом внутри.
func TestLoadConfigWhitespaceOnlyEndpointRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "   ",
		"GOTCHA_AGENT_KEY":      "pk",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_ENDPOINT должен ронять старт")
	}
}

// TestLoadConfigWhitespaceOnlyKeyRejected — тот же контракт для ключа: он
// используется как есть в заголовке Authorization (sender.go), поэтому
// пробельное значение обязано быть отказом старта, а не пустым Bearer.
func TestLoadConfigWhitespaceOnlyKeyRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://g.example",
		"GOTCHA_AGENT_KEY":      "   ",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_KEY должен ронять старт")
	}
}

// TestLoadConfigTrimsKeyCACertLabels — Key/CACert/Environment/Role/Hostname
// раньше не триммились вовсе. Key напрямую уходит в заголовок Authorization:
// "Bearer abc " с хвостовым пробелом — это НЕ тот же Bearer-токен, что
// "Bearer abc", и сервер отклонит его как неизвестный ключ без единого
// понятного сообщения в логах агента.
func TestLoadConfigTrimsKeyCACertLabels(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":    "https://g.example",
		"GOTCHA_AGENT_KEY":         "abc ",
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

// TestLoadConfigHostnameTrimmedConsistently — GOTCHA_AGENT_HOSTNAME — это
// identity-ключ хоста в проде (host.name resource-атрибут, ключ карты на
// приёме, см. докблок Hostname в config.go). "web-1", "web-1 " и " web-1"
// обязаны дать один и тот же host.name — иначе оператор, «поправив» пробел
// на живом инстансе, тихо переименовал бы хост в новый вместо того же самого
// (потеря меток/порогов/зависимостей на старом ключе).
func TestLoadConfigHostnameTrimmedConsistently(t *testing.T) {
	for _, raw := range []string{"web-1", "web-1 ", " web-1", "\tweb-1\n"} {
		cfg, err := LoadConfig(env(map[string]string{
			"GOTCHA_AGENT_ENDPOINT": "https://g.example",
			"GOTCHA_AGENT_KEY":      "pk",
			"GOTCHA_AGENT_HOSTNAME": raw,
		}))
		if err != nil {
			t.Fatalf("GOTCHA_AGENT_HOSTNAME=%q: LoadConfig: %v", raw, err)
		}
		if cfg.Hostname != "web-1" {
			t.Errorf("GOTCHA_AGENT_HOSTNAME=%q: Hostname = %q, want %q", raw, cfg.Hostname, "web-1")
		}
	}
}

// TestLoadConfigKeyTrimReflectedInBearerHeader — GOTCHA_AGENT_KEY → Config.Key
// → заголовок Authorization (sender.go подставляет cfg.Key как есть, строкой
// "Bearer "+cfg.Key, без собственного тримминга): "abc " с хвостовым пробелом
// обязан дать ровно "Bearer abc", а не "Bearer abc ".
//
// Собирается http.Request тем же способом, что и Send() в sender.go
// (http.NewRequestWithContext + req.Header.Set("Authorization", "Bearer
// "+cfg.Key)), а не через реальный HTTP round-trip: net/http/textproto
// обрезает OWS (optional whitespace) у значений заголовков на приёмной
// стороне при разборе запроса, так что httptest.Server увидел бы уже
// нормализованное значение и не отличил бы тримминг в LoadConfig от его
// отсутствия — round-trip через httptest здесь маскировал бы регресс,
// а не проверял его. Header.Get читает значение из Request.Header
// (map[string][]string) напрямую, до какой-либо сериализации на провод.
func TestLoadConfigKeyTrimReflectedInBearerHeader(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT": "https://g.example",
		"GOTCHA_AGENT_KEY":      "abc ",
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
