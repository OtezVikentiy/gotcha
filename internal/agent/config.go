package agent

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/baseurl"
	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Публичный контракт GOTCHA_AGENT_* — поля не переименовывать, ломает совместимость с агентом.
type Config struct {
	Endpoint           string        // базовый URL инстанса, без пути
	Key                string        // публичный ключ проекта (Bearer)
	Hostname           string        // переопределение host.name; "" — os.Hostname в Run
	CACert             string        // путь к PEM CA (самоподписанные инстансы)
	Interval           time.Duration // 10s..5m (env — целые секунды, 10..300), дефолт 30s
	InsecureSkipVerify bool          // крайнее средство; рекомендуемый путь — CACert
	Environment        string        // resource-метка deployment.environment; "" — не эмитится
	Role               string        // resource-метка host.role; "" — не эмитится
}

const (
	defaultInterval = 30 * time.Second
	minIntervalSecs = 10  // ниже — самоDoS ключом по ingest
	maxIntervalSecs = 300 // выше — «тишина» порогов ложно срабатывает

	// То же значение, что maxIntervalSecs, но в Duration — run_test.go
	// использует его напрямую, в обход LoadConfig.
	maxInterval = maxIntervalSecs * time.Second
)

// Имя функции важно: internal/guards ищет вызовы intNum по имени для
// проверки конвенции единиц — переименование сломает guard молча.
func intNum(name, raw string) (value int, set bool, err error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, strconv.IntSize)
	if err != nil {
		return 0, true, fmt.Errorf("%s: %w", name, err)
	}
	return int(n), true, nil
}

// getenv — для тестов без t.Setenv; environ перечисляет всё окружение
// отдельно, чтобы поймать неизвестные GOTCHA_AGENT_* имена.
func LoadConfig(getenv func(string) string, environ func() []string) (Config, error) {
	// AgentOwned, не CheckRenamedAll: чужие серверные имена агент не читает.
	// Вызов до разбора значений — иначе устаревшее имя тихо станет дефолтом.
	if err := envcontract.CheckRenamedScoped(getenv, envcontract.AgentOwned); err != nil {
		return Config{}, err
	}
	cfg := Config{
		// Trim до baseurl.Normalize — сам Normalize пробелы не трогает, это
		// забота вызывающего (как и для GOTCHA_BASE_URL и соседей).
		Endpoint: strings.TrimSpace(getenv("GOTCHA_AGENT_ENDPOINT")),
		Key:      strings.TrimSpace(getenv("GOTCHA_AGENT_INGEST_KEY")),
		// host.name уходит как есть в приём — identity-ключ хоста: " web-1 " и
		// "web-1" — РАЗНЫЕ хосты, здесь тримминг обязателен.
		Hostname:    strings.TrimSpace(getenv("GOTCHA_AGENT_HOSTNAME")),
		CACert:      strings.TrimSpace(getenv("GOTCHA_AGENT_CA_CERT")),
		Interval:    defaultInterval,
		Environment: strings.TrimSpace(getenv("GOTCHA_AGENT_ENVIRONMENT")),
		Role:        strings.TrimSpace(getenv("GOTCHA_AGENT_ROLE")),
	}
	if cfg.Endpoint == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_ENDPOINT is required")
	}
	// Ошибка Normalize пробрасывается как есть — общая формулировка увела бы
	// от настоящей причины (например лишний query при валидной схеме/хосте).
	normalized, err := baseurl.Normalize("GOTCHA_AGENT_ENDPOINT", cfg.Endpoint)
	if err != nil {
		return Config{}, err
	}
	cfg.Endpoint = normalized
	if cfg.Key == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_INGEST_KEY is required")
	}
	// Целое число секунд, не duration-строка ("30s" не пройдёт intNum).
	if raw := strings.TrimSpace(getenv("GOTCHA_AGENT_INTERVAL_SECONDS")); raw != "" {
		seconds, _, err := intNum("GOTCHA_AGENT_INTERVAL_SECONDS", raw)
		if err != nil {
			return Config{}, err
		}
		if seconds < minIntervalSecs || seconds > maxIntervalSecs {
			return Config{}, fmt.Errorf("GOTCHA_AGENT_INTERVAL_SECONDS must be within %d..%d, got %d", minIntervalSecs, maxIntervalSecs, seconds)
		}
		cfg.Interval = time.Duration(seconds) * time.Second
	}
	v, set, err := parseBool("GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY", getenv("GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY"))
	if err != nil {
		return Config{}, err
	}
	if set {
		cfg.InsecureSkipVerify = v
	}
	if err := checkUnknownAgentEnvVars(environ); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Только префикс GOTCHA_AGENT_, не любой GOTCHA_* — чужие серверные
// переменные в общем .env агент не читает и не должен по ним отказывать.
func checkUnknownAgentEnvVars(environ func() []string) error {
	var renamed, unknown []string
	for _, kv := range environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GOTCHA_AGENT_") {
			continue
		}
		if envcontract.Known[name] {
			continue
		}
		if _, wasRenamed := envcontract.Renamed[name]; wasRenamed {
			renamed = append(renamed, name)
			continue
		}
		unknown = append(unknown, name)
	}
	var errs []error
	if err := envcontract.RenamedError(renamed); err != nil {
		errs = append(errs, err)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Errorf("unknown environment variable(s) in the GOTCHA_AGENT_ namespace, check for typos: %s", strings.Join(unknown, ", ")))
	}
	return errors.Join(errs...)
}

// Тот же разбор, что parseBool в cmd/gotcha/config.go — синхронизируется
// вручную, agent не может импортировать package main.
func parseBool(name, raw string) (value bool, set bool, err error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return false, false, nil
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("%s: invalid boolean %q (want 1/0/true/false/yes/no/on/off)", name, raw)
	}
}
