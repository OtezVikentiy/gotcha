// Package agent — ядро gotcha-agent: сбор хост-метрик через gopsutil,
// сборка OTLP-экспорта и push на инстанс Gotcha с буферизацией недоставленного.
// Вся логика здесь (cmd/gotcha-agent — тонкая обвязка): пакет входит в
// BACK-агрегат покрытия, а точка входа — в щадящую CMD-группу.
package agent

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config — публичный контракт GOTCHA_AGENT_* (фиксируется набело, спека §1.4).
type Config struct {
	Endpoint           string        // базовый URL инстанса, без пути
	Key                string        // публичный ключ проекта (Bearer)
	Hostname           string        // переопределение host.name; "" — os.Hostname в Run
	CACert             string        // путь к PEM CA (самоподписанные инстансы)
	Interval           time.Duration // 10s..5m, дефолт 30s
	InsecureSkipVerify bool          // крайнее средство; рекомендуемый путь — CACert
	Environment        string        // resource-метка deployment.environment; "" — не эмитится
	Role               string        // resource-метка host.role; "" — не эмитится
}

const (
	defaultInterval = 30 * time.Second
	minInterval     = 10 * time.Second // ниже — самоDoS ключом по ingest
	maxInterval     = 5 * time.Minute  // выше — «тишина» порогов ложно срабатывает
)

// LoadConfig читает окружение. getenv параметром — детерминированные тесты
// без t.Setenv (тот же приём, что loadConfig в cmd/gotcha).
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		// Endpoint: пробелы по краям обрезаются ПЕРЕД срезом хвостовой "/" —
		// иначе "https://g.example/ " (пробел после слэша) прошёл бы TrimRight
		// как есть и оставил пробел на конце базового URL.
		Endpoint: strings.TrimRight(strings.TrimSpace(getenv("GOTCHA_AGENT_ENDPOINT")), "/"),
		Key:      strings.TrimSpace(getenv("GOTCHA_AGENT_KEY")),
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
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_ENDPOINT must be an http(s) URL, got %q", cfg.Endpoint)
	}
	if cfg.Key == "" {
		return Config{}, fmt.Errorf("GOTCHA_AGENT_KEY is required")
	}
	if raw := getenv("GOTCHA_AGENT_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("GOTCHA_AGENT_INTERVAL: %w", err)
		}
		if d < minInterval || d > maxInterval {
			return Config{}, fmt.Errorf("GOTCHA_AGENT_INTERVAL must be within %s..%s, got %s", minInterval, maxInterval, d)
		}
		cfg.Interval = d
	}
	v, set, err := parseBool("GOTCHA_AGENT_TLS_SKIP_VERIFY", getenv("GOTCHA_AGENT_TLS_SKIP_VERIFY"))
	if err != nil {
		return Config{}, err
	}
	if set {
		cfg.InsecureSkipVerify = v
	}
	return cfg, nil
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
