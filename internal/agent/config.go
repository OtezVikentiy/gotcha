// Package agent — ядро gotcha-agent: сбор хост-метрик через gopsutil,
// сборка OTLP-экспорта и push на инстанс Gotcha с буферизацией недоставленного.
// Вся логика здесь (cmd/gotcha-agent — тонкая обвязка): пакет входит в
// BACK-агрегат покрытия, а точка входа — в щадящую CMD-группу.
package agent

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/baseurl"
	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Config — публичный контракт GOTCHA_AGENT_* (фиксируется набело, спека §1.4).
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

	// maxInterval — то же значение диапазона, что maxIntervalSecs, в
	// исходном представлении Config.Interval (time.Duration): run_test.go
	// использует его напрямую, минуя LoadConfig/GOTCHA_AGENT_INTERVAL_SECONDS.
	maxInterval = maxIntervalSecs * time.Second
)

// intNum разбирает голое целое число секунд. Название функции — не
// совпадение с cmd/gotcha/config.go: internal/guards/env_example_test.go
// (numericReaderFuncs) ищет вызовы именно с этим именем, чтобы применить
// конвенцию единиц измерения к голым числовым переменным (здесь единица
// уже в имени, суффикс _SECONDS). Общий код с cmd/gotcha не заводится —
// agent не может импортировать package main. На пустом значении возвращает
// set=false (переменная не задана, у вызывающего кода свой дефолт); на
// ошибке разбора НЕ возвращает частичный результат strconv — так "30s"
// (старый duration-формат) не может быть по ошибке принято как правдоподобное
// число вместо явного отказа.
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

// LoadConfig читает окружение. getenv параметром — детерминированные тесты
// без t.Setenv (тот же приём, что loadConfig в cmd/gotcha). environ
// перечисляет всё окружение процесса ("KEY=VALUE" на строку, как
// os.Environ()) — нужен отдельно от getenv (интерфейс «дай значение по
// ключу» не умеет перечислить, что вообще задано) для проверки неизвестных
// имён в конце этой функции; в проде — os.Environ, в тестах — фикстура.
func LoadConfig(getenv func(string) string, environ func() []string) (Config, error) {
	// CheckRenamedScoped на envcontract.AgentOwned (три свои пары), не
	// CheckRenamedAll: агент не должен отказывать на устаревших СЕРВЕРНЫХ
	// именах в общем .env одного хоста — эти переменные он никогда не
	// читает, отказ по ним не защита, а самоуправство (ops-review E3 T8
	// круг 1). Идёт ДО любого разбора значений — иначе валидный когда-то
	// "30s" под старым именем успел бы разобраться (не как секунды, но как
	// молчаливый дефолт) прежде, чем оператор узнает, что имя устарело.
	if err := envcontract.CheckRenamedScoped(getenv, envcontract.AgentOwned); err != nil {
		return Config{}, err
	}
	cfg := Config{
		// Endpoint: пробелы по краям обрезаются здесь, ДО baseurl.Normalize
		// ниже — иначе "https://g.example/ " (пробел после слэша) прошёл бы
		// TrimRight внутри Normalize как есть и оставил бы пробел на конце
		// базового URL (Normalize сам пробелы не трогает — это забота
		// вызывающего кода, как и у GOTCHA_BASE_URL/GOTCHA_TELEGRAM_API_BASE/
		// GOTCHA_PROBE_SERVER_URL в cmd/gotcha/config.go).
		Endpoint: strings.TrimSpace(getenv("GOTCHA_AGENT_ENDPOINT")),
		Key:      strings.TrimSpace(getenv("GOTCHA_AGENT_INGEST_KEY")),
		// Hostname — identity-ключ хоста в проде: run.go кладёт его как есть в
		// OTLP-атрибут host.name (emit.go), а приём (internal/ingest/otlp.go)
		// использует сырой host.name ключом карты хостов без своего тримминга.
		// " web-1 " и "web-1" — два РАЗНЫХ ключа: оператор, «поправив» пробел
		// на живом инстансе, получил бы НОВЫЙ хост вместо переименования
		// старого, с потерей меток/порогов/зависимостей. Серверная нормализация
		// host.name — отдельная работа (не тут, приём принимает значения не
		// только от этого агента); здесь только не порождать проблему на входе.
		Hostname:    strings.TrimSpace(getenv("GOTCHA_AGENT_HOSTNAME")),
		CACert:      strings.TrimSpace(getenv("GOTCHA_AGENT_CA_CERT")),
		Interval:    defaultInterval,
		Environment: strings.TrimSpace(getenv("GOTCHA_AGENT_ENVIRONMENT")),
		Role:        strings.TrimSpace(getenv("GOTCHA_AGENT_ROLE")),
	}
	if cfg.Endpoint == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_ENDPOINT is required")
	}
	// baseurl.Normalize — тот же хелпер, что GOTCHA_BASE_URL/
	// GOTCHA_TELEGRAM_API_BASE/GOTCHA_PROBE_SERVER_URL в cmd/gotcha/config.go:
	// схема и хост обязательны, query/fragment запрещены, хвостовые слэши
	// срезаются (до этой правки Endpoint слэш срезал, но query не проверял).
	// Ошибка Normalize отдаётся ОПЕРАТОРУ дословно, а не заменяется общим
	// «must be an http(s) URL»: у Normalize свой текст на каждый класс
	// проблемы (нет схемы/хоста, лишние query/fragment) с именем переменной
	// уже внутри (см. её докблок в internal/baseurl) — сервер (GOTCHA_BASE_URL
	// и соседи, cmd/gotcha/config.go) отдаёт эту ошибку так же напрямую.
	// Общая формулировка молча подменяла точную причину («…must not carry a
	// query or fragment») на неточную («…must be an http(s) URL») для
	// адреса, который http(s)-схему и хост как раз имеет.
	normalized, err := baseurl.Normalize("GOTCHA_AGENT_ENDPOINT", cfg.Endpoint)
	if err != nil {
		return Config{}, err
	}
	cfg.Endpoint = normalized
	if cfg.Key == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_INGEST_KEY is required")
	}
	// GOTCHA_AGENT_INTERVAL_SECONDS — целое число секунд, не duration-строка:
	// intNum отказывает разбор на "30s" вместо того, чтобы молча принять его
	// как значение, — единственная duration-строка продукта раньше не несла
	// единицу измерения в самом имени, в отличие от шести серверных *_SECONDS.
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

// checkUnknownAgentEnvVars отказывает старту, если в окружении процесса
// присутствует переменная с префиксом GOTCHA_AGENT_, которую не читает ни
// эта функция, ни какая-либо другая (envcontract.Known — общий реестр,
// объединяющий агентские и серверные имена, см. её докблок в
// internal/envcontract/known.go).
//
// ТОЛЬКО свой префикс, а не любая GOTCHA_*: чужая серверная переменная
// (GOTCHA_PG_DSN и т.п.) в общем `.env` одного хоста — легитимный сосед по
// файлу, агент её никогда не читал и отказ по ней был бы самоуправством
// (тот же принцип, что у envcontract.CheckRenamedScoped выше — см. её
// докблок). Но опечатка ВНУТРИ своего же префикса — другое дело: агент
// штатно стоит на удалённом хосте ОДИН, там нет ни сервера, ни его проверки
// неизвестных имён (checkUnknownEnvVars, cmd/gotcha/config.go) — без этой
// проверки install.sh --check подтверждал бы битый конфиг агента как
// "config OK", а опечатка (например GOTCHA_AGENT_INTERVAL_SECOND без "S" на
// конце) молча превращалась бы в «переменная не задана», то есть в тихий
// дефолт вместо отказа старта.
func checkUnknownAgentEnvVars(environ func() []string) error {
	var unknown []string
	for _, kv := range environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GOTCHA_AGENT_") {
			continue
		}
		// Пустое значение — та же трактовка «не задано», что и везде в
		// конфиге агента (см. TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart):
		// docker-compose штатно прокидывает объявленные, но не заданные
		// переменные пустой строкой — старое ИМЯ переменной, оставшееся в
		// compose-файле неиспользуемым, не повод отказывать старту.
		if value == "" {
			continue
		}
		if envcontract.Known[name] {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown environment variable(s) in the GOTCHA_AGENT_ namespace, check for typos: %s", strings.Join(unknown, ", "))
}

// parseBool — тот же разбор булевых значений env, что у parseBool в
// cmd/gotcha/config.go: trim + lower, истина 1/true/yes/on, ложь
// 0/false/no/off, пустая строка — «не задано». agent не может импортировать
// package main, поэтому набор синхронизируется вручную, а не общим кодом.
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
