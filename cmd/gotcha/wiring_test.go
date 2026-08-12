package main

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// Точка входа состоит из проводки, но решения в ней есть, и они меняют
// поведение продукта: кому уйдут детали события, запускать ли оценщики, каким
// ключом подписывается oauth-cookie. Раньше пакет не входил в профиль покрытия
// вовсе — эти решения не проверялись ничем.

// TestRunEvaluatorsDefaultsAndExplicit: молчаливое «не включать» уже приводило
// к «правило включено и не срабатывает никогда», поэтому дефолт и явный выбор
// различаются намеренно.
func TestRunEvaluatorsDefaultsAndExplicit(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name         string
		cfg          Config
		wantRun      bool
		wantExplicit bool
	}{
		{"по умолчанию — включены", Config{}, true, false},
		{"явно включены", Config{RunEvaluators: &yes}, true, true},
		{"явно выключены", Config{RunEvaluators: &no}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runEvaluators(tc.cfg); got != tc.wantRun {
				t.Errorf("runEvaluators = %v, want %v", got, tc.wantRun)
			}
			if got := runEvaluatorsExplicit(tc.cfg); got != tc.wantExplicit {
				t.Errorf("runEvaluatorsExplicit = %v, want %v: в режимах без аптайма "+
					"дефолта нет, и «не задано» нельзя путать с «включено»", got, tc.wantExplicit)
			}
		})
	}
}

// TestDeriveCookieKeyIsDomainSeparated: подключ для подписи oauth-cookie не
// должен совпадать с мастер-секретом и обязан быть детерминированным —
// иначе рестарт инстанса рвёт все начатые входы через провайдера.
func TestDeriveCookieKeyIsDomainSeparated(t *testing.T) {
	if got := deriveCookieKey(""); got != "" {
		t.Errorf("пустой мастер-секрет дал ключ %q — web-слой различает эти случаи сам", got)
	}
	const master = "master-secret-value"
	first := deriveCookieKey(master)
	if first == "" {
		t.Fatal("подключ пуст при заданном мастер-секрете")
	}
	if first == master {
		t.Error("подключ совпал с мастер-секретом — доменного разделения нет")
	}
	if second := deriveCookieKey(master); second != first {
		t.Error("подключ недетерминирован: рестарт оборвал бы все начатые входы через провайдера")
	}
	if other := deriveCookieKey(master + "x"); other == first {
		t.Error("разные мастер-секреты дали один подключ")
	}
}

// TestDetailPolicyFollowsRecipient: детали события уходят по доверенности
// ПОЛУЧАТЕЛЯ, а не по виду канала — на этом строится трансграничный гейт.
func TestDetailPolicyFollowsRecipient(t *testing.T) {
	cfg := Config{
		BaseURL:           "https://gotcha.example.com",
		TrustedRecipients: []string{"acme.example"},
	}
	policy := detailPolicy(cfg)

	if !policy.AllowsDetails(alert.Channel{Kind: alert.ChannelEmail, Target: "ops@gotcha.example.com"}) {
		t.Error("получателю на домене инстанса детали не ушли")
	}
	if !policy.AllowsDetails(alert.Channel{Kind: alert.ChannelEmail, Target: "ops@acme.example"}) {
		t.Error("получателю из доверенного списка детали не ушли")
	}
	if policy.AllowsDetails(alert.Channel{Kind: alert.ChannelEmail, Target: "stranger@mail.example"}) {
		t.Error("детали ушли постороннему получателю — гейт по получателю не работает")
	}
	if policy.AllowsDetails(alert.Channel{Kind: alert.ChannelTelegram, Target: "12345"}) {
		t.Error("детали ушли в Telegram: получателя разобрать нечем, значит и доверять нечему")
	}

	open := detailPolicy(Config{BaseURL: cfg.BaseURL, ExternalChannelDetails: true})
	if !open.AllowsDetails(alert.Channel{Kind: alert.ChannelTelegram, Target: "12345"}) {
		t.Error("при GOTCHA_EXTERNAL_CHANNEL_DETAILS=true детали обязаны уходить всем")
	}
	// Лог о действующей политике не должен падать ни в одном режиме.
	logDetailPolicy(cfg)
	logDetailPolicy(Config{BaseURL: cfg.BaseURL, ExternalChannelDetails: true})
}

// TestSetupLoggingAcceptsKnownLevels: нераспознанное значение не должно менять
// поведение молча — апгрейд с новым параметром иначе тихо поднимал бы
// детализацию логов на проде.
func TestSetupLoggingAcceptsKnownLevels(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "warning", "error", "nonsense"} {
		for _, format := range []string{"", "json", "text", "nonsense"} {
			setupLogging(level, format) // не должно паниковать ни на одном сочетании
		}
	}
}

// TestAutoMaxBufferBytesSafeUnderHeapCeiling: находка P0-1 — дефолтный
// docker-compose.yml (mem_limit 1g, GOTCHA_MAX_BUFFER_BYTES не задан) даёт
// потолок кучи 819 МиБ (0.8×1024 МиБ), а пять буферов-«единиц» по flat
// defaultMaxBufBytes=256 МиБ (событие+SpanWriter×2+метрика+профиль) суммарно
// весят 1.25 ГиБ — больше потолка. Авто-дефолт обязан вывести per-writer-cap,
// при котором сумма пяти единиц укладывается в потолок с запасом.
func TestAutoMaxBufferBytesSafeUnderHeapCeiling(t *testing.T) {
	memLimit1g := int64(1024 << 20)
	heapCeiling := int64(float64(memLimit1g) * 0.8) // как memlimit.heapTarget

	perWriterCap := autoMaxBufferBytes(heapCeiling)
	if perWriterCap <= 0 {
		t.Fatalf("autoMaxBufferBytes(%d) = %d, хочу положительный per-writer-cap", heapCeiling, perWriterCap)
	}
	const flatDefault = 256 << 20
	if perWriterCap >= flatDefault {
		t.Fatalf("autoMaxBufferBytes(%d) = %d >= flat-дефолт %d — авто-дефолт не уже flat, "+
			"находка не закрыта", heapCeiling, perWriterCap, flatDefault)
	}
	sum := perWriterCap * 5 // event(1) + SpanWriter(2) + metric(1) + profile(1)
	if sum > heapCeiling {
		t.Fatalf("5 единиц по %d = %d байт превышают потолок кучи %d — дефолтная поставка всё ещё может OOM",
			perWriterCap, sum, heapCeiling)
	}
}

// TestAutoMaxBufferBytesNoLimitFallsBackToPackageDefault: bare-metal без
// cgroup (memlimit вернул ErrNoLimit → applyMemoryLimit вернул 0) не должен
// менять поведение — это не регресс, а прежний flat-дефолт пакета-писателя.
func TestAutoMaxBufferBytesNoLimitFallsBackToPackageDefault(t *testing.T) {
	for _, heapCeiling := range []int64{0, -1} {
		if got := autoMaxBufferBytes(heapCeiling); got != 0 {
			t.Errorf("autoMaxBufferBytes(%d) = %d, хочу 0 (сигнал «оставь flat-дефолт пакета»)", heapCeiling, got)
		}
	}
}

// TestEffectiveMaxBufferBytesRespectsExplicitOverride: явный
// GOTCHA_MAX_BUFFER_BYTES обязан побеждать авто-дефолт в обе стороны — и
// когда оператор просит больше, и когда меньше того, что вывел бы авто-режим.
func TestEffectiveMaxBufferBytesRespectsExplicitOverride(t *testing.T) {
	const heapCeiling = 800 << 20
	const explicit = 24 << 20 // как в docker-compose.small.yml
	if got := effectiveMaxBufferBytes(explicit, heapCeiling); got != explicit {
		t.Errorf("effectiveMaxBufferBytes(explicit=%d, heap=%d) = %d, явный GOTCHA_MAX_BUFFER_BYTES проигнорирован",
			explicit, heapCeiling, got)
	}
	if got := effectiveMaxBufferBytes(explicit, 0); got != explicit {
		t.Errorf("effectiveMaxBufferBytes(explicit=%d, heap=0) = %d, явный override не должен зависеть от лимита",
			explicit, got)
	}
	if got, want := effectiveMaxBufferBytes(0, heapCeiling), autoMaxBufferBytes(heapCeiling); got != want {
		t.Errorf("effectiveMaxBufferBytes(0, %d) = %d, хочу авто-дефолт %d", heapCeiling, got, want)
	}
	if got := effectiveMaxBufferBytes(0, 0); got != 0 {
		t.Errorf("effectiveMaxBufferBytes(0, 0) = %d, хочу 0 (flat-дефолт пакета, не регресс)", got)
	}
}

// TestVersionRequested: флаг версии распознаётся до любой инициализации —
// `gotcha --version` обязан работать без баз и конфигурации.
func TestVersionRequestedForms(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}, {"--mode=web", "--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"--mode=web"}, {"--versionx"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%v) = true", args)
		}
	}
	_ = strings.TrimSpace("")
}
