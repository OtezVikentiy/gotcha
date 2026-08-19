package agent

import (
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
