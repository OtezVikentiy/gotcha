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

// env возвращает и getenv, и environ для LoadConfig — оба вида доступа к
// одной и той же карте. Возврат двух значений позволяет писать
// LoadConfig(env(vars)) без распаковки: Go передаёт результат
// многозначного вызова как оба аргумента, если это единственный аргумент
// вызова.
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

// environFrom — вариант env() для тестов, которым нужен только environ
// (проверка checkUnknownAgentEnvVars напрямую), в том же стиле, что
// environFrom в cmd/gotcha/unknown_env_vars_test.go.
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
		"без endpoint":    {"GOTCHA_AGENT_INGEST_KEY": "pk"},
		"без key":         {"GOTCHA_AGENT_ENDPOINT": "https://g"},
		"кривой endpoint": {"GOTCHA_AGENT_ENDPOINT": "ftp://g", "GOTCHA_AGENT_INGEST_KEY": "pk"},
		"interval мал":    {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "5"},
		"interval велик":  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "301"},
		"interval мусор":  {"GOTCHA_AGENT_ENDPOINT": "https://g", "GOTCHA_AGENT_INGEST_KEY": "pk", "GOTCHA_AGENT_INTERVAL_SECONDS": "щедро"},
		// E3 T8: тип сменился с duration-строки на целые секунды — старый
		// формат "30s" (валидный до переименования) обязан быть отказом
		// разбора, а не тихо превратиться в другое значение.
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

// TestLoadConfigTLSSkipVerifyAcceptedSpellings — GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY
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

// TestLoadConfigTLSSkipVerifyRejectsInvalid — мусор в булевой переменной не
// должен молча превращаться в false: см. TestLoadConfigRunEvaluatorsRejectsInvalid
// в cmd/gotcha за тем же контрактом.
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
			"GOTCHA_AGENT_ENDPOINT":   raw,
			"GOTCHA_AGENT_INGEST_KEY": "pk",
		})); err == nil {
			t.Errorf("GOTCHA_AGENT_ENDPOINT=%q: want error, got nil", raw)
		}
	}
}

// TestLoadConfigEndpointNormalizeErrorPassedThroughVerbatim — m1 (финальное
// ревью): ошибка baseurl.Normalize отдаётся оператору дословно, а не
// заменяется общим «must be an http(s) URL» — у Normalize свой точный текст
// на каждый класс проблемы (см. её докблок), и для адреса СО схемой и
// хостом, но с query, общая формулировка называла бы неверную причину.
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

// TestLoadConfigWhitespaceOnlyEndpointRejected — эндпоинт обязателен; строка
// из одних пробелов после тримминга становится пустой и обязана давать ту же
// ошибку, что полностью отсутствующая переменная, а не URL с пробелом внутри.
func TestLoadConfigWhitespaceOnlyEndpointRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "   ",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_ENDPOINT должен ронять старт")
	}
}

// TestLoadConfigWhitespaceOnlyKeyRejected — тот же контракт для ключа: он
// используется как есть в заголовке Authorization (sender.go), поэтому
// пробельное значение обязано быть отказом старта, а не пустым Bearer.
func TestLoadConfigWhitespaceOnlyKeyRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "   ",
	})); err == nil {
		t.Fatal("пробельный GOTCHA_AGENT_INGEST_KEY должен ронять старт")
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

// TestLoadConfigHostnameTrimmedConsistently — GOTCHA_AGENT_HOSTNAME — это
// identity-ключ хоста в проде (host.name resource-атрибут, ключ карты на
// приёме, см. докблок Hostname в config.go). "web-1", "web-1 " и " web-1"
// обязаны дать один и тот же host.name — иначе оператор, «поправив» пробел
// на живом инстансе, тихо переименовал бы хост в новый вместо того же самого
// (потеря меток/порогов/зависимостей на старом ключе).
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

// TestLoadConfigKeyTrimReflectedInBearerHeader — GOTCHA_AGENT_INGEST_KEY → Config.Key
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

// TestLoadConfigIntervalSecondsApplied — GOTCHA_AGENT_INTERVAL_SECONDS=30
// (целое число секунд, канонический контракт после переименования) даёт
// Interval=30s, тот же дефолт, что и без переменной вовсе.
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

// TestLoadConfigIntervalSecondsBoundaryError — "5" (ниже минимума 10) и
// "301" (выше максимума 300) обязаны называть границы диапазона в тексте
// ошибки, а не просто отказывать молча.
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

// TestLoadConfigRenamedEnvVarFailsStart — envcontract.AgentOwned (три свои
// пары, НЕ весь реестр — агент не отвечает за 27 серверных переменных,
// которые никогда не читает; см. докблок LoadConfig и AgentOwned в
// internal/envcontract/renamed.go): КАЖДОЕ своё старое имя с непустым
// значением роняет старт агента, сообщение называет И старое, И новое имя.
// Подтест на КАЖДУЮ пару (t.Run по старому имени), а не одна проверка на
// первую попавшуюся — иначе неоднородный баг в envcontract.CheckRenamedScoped,
// срабатывающий не на всех именах, прошёл бы незамеченным.
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

// TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart — пустое значение
// старого имени не роняет старт: docker-compose штатно прокидывает
// объявленные, но не заданные переменные пустой строкой. ENDPOINT/KEY заданы
// явно и валидно — иначе на пустом окружении LoadConfig упал бы по другой,
// не связанной с переименованием причине, и тест не отличил бы «прошёл
// renamed-check» от «упал раньше него».
func TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart(t *testing.T) {
	old := sortedAgentOwnedOldNames()[0]
	if _, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
		old:                       "",
	})); err != nil {
		t.Errorf("LoadConfig с пустым устаревшим %s: %v, want nil (пустое значение легитимно)", old, err)
	}
}

// TestLoadConfigIgnoresOutOfScopeRenamedNames — старое СЕРВЕРНОЕ имя (не
// входящее в envcontract.AgentOwned), стоящее с непустым значением в общем
// .env хоста, не должно ронять старт агента: он никогда его не читал ни до,
// ни после переименования, и отказ по нему был бы самоуправством, а не
// защитой — агент проверяет ТОЛЬКО свои старые имена, а не весь реестр из
// 30 записей, включая 27 чужих. ENDPOINT/KEY заданы явно и
// валидно, чтобы тест проверял именно эту ветку, а не общий отказ на их
// отсутствие.
func TestLoadConfigIgnoresOutOfScopeRenamedNames(t *testing.T) {
	agentOwned := map[string]bool{}
	for _, old := range envcontract.AgentOwned {
		agentOwned[old] = true
	}
	outOfScope := ""
	for old := range envcontract.Renamed {
		if !agentOwned[old] {
			outOfScope = old
			break
		}
	}
	if outOfScope == "" {
		t.Fatal("обход ослеп: в envcontract.Renamed не нашлось имени вне AgentOwned")
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

// agentRenamedEnvVarNewNameChecks — новое имя → тестовое значение и читатель
// соответствующего поля Config, для ТРЁХ переименований, которые принадлежит
// internal/agent (не cmd/gotcha — там у Config нет и не может быть полей
// под эти три переменные, см. agentOwnedRenamedNewNames в
// cmd/gotcha/renamed_env_contract_test.go). Регрессия на то, что
// переименование не сломало применение НОВОГО имени.
var agentRenamedEnvVarNewNameChecks = map[string]struct {
	value string
	get   func(Config) string
}{
	"GOTCHA_AGENT_INTERVAL_SECONDS":         {"120", func(c Config) string { return strconv.Itoa(int(c.Interval.Seconds())) }},
	"GOTCHA_AGENT_INGEST_KEY":               {"renamed-regression-key", func(c Config) string { return c.Key }},
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": {"true", func(c Config) string { return strconv.FormatBool(c.InsecureSkipVerify) }},
}

// TestAgentRenamedEnvVarNewNameChecksComplete — agentRenamedEnvVarNewNameChecks
// обязана содержать РОВНО новые имена envcontract.AgentOwned (единственный
// источник — тот же срез, что LoadConfig передаёт в CheckRenamedScoped) — ни
// лишних, ни пропущенных. То же назначение, что
// TestRenamedEnvVarNewNameChecksComplete в cmd/gotcha: без этой сверки
// новая агентская пара тихо осталась бы без регрессии.
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

// TestLoadConfigRenamedEnvVarNewNameStillApplies — подтест на КАЖДУЮ запись
// agentRenamedEnvVarNewNameChecks, реальная итерация (а не одна проверка
// вручную выбранной пары).
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
