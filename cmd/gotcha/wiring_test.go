package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
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

// TestEvaluatorsDisabledWarningNamesAllSixCycles: GOTCHA_EVALUATORS_ENABLED гейтит
// ШЕСТЬ фоновых циклов (metric/trace(perf)/profile/host-оценщики + slo.Evaluator
// + escalation.Scheduler, см. startEvaluators), но предупреждение при старте
// раньше называло только четыре — slo и эскалация молчали о себе так же, как
// правило по метрике молчит без этого предупреждения (W3-D, запись 8 = находка
// W3-C). Оператор раздельного развёртывания web+ingest без
// GOTCHA_EVALUATORS_ENABLED не узнавал, что SLO-алерты и эскалация ВСЕХ пяти
// источников инцидентов (не только SLO) тоже не работают.
func TestEvaluatorsDisabledWarningNamesAllSixCycles(t *testing.T) {
	for _, want := range []string{"metric", "profile", "host", "slo", "escalation"} {
		if !strings.Contains(evaluatorsDisabledWarning, want) {
			t.Errorf("evaluatorsDisabledWarning не упоминает %q: %s", want, evaluatorsDisabledWarning)
		}
	}
	// trace-пакет отвечает за регрессии производительности — в тексте
	// предупреждения (и в UI) он называется "performance"/"regression", не
	// "trace" (см. соседние Warn/лог сообщения того же файла).
	if !strings.Contains(evaluatorsDisabledWarning, "regression") {
		t.Errorf("evaluatorsDisabledWarning не упоминает регрессии производительности (trace.Evaluator): %s",
			evaluatorsDisabledWarning)
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
		t.Error("при GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true детали обязаны уходить всем")
	}
	// Лог о действующей политике не должен падать ни в одном режиме.
	logDetailPolicy(cfg)
	logDetailPolicy(Config{BaseURL: cfg.BaseURL, ExternalChannelDetails: true})
}

// TestLogDetailPolicyLogsTrustedRecipients — m7 (финальное ревью): разбор
// GOTCHA_TRUSTED_RECIPIENTS не отказывает старт ни на одном значении
// (невалидное имя просто ни с чем не совпадёт), так что лог на старте —
// единственная диагностика опечатки в списке. Список обязан попасть в лог в
// ОБЕИХ ветках logDetailPolicy, а не только когда список реально фильтрует
// доставку (default) — GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true не должен
// прятать распознанный список от оператора.
func TestLogDetailPolicyLogsTrustedRecipients(t *testing.T) {
	cfg := Config{
		BaseURL:           "https://gotcha.example.com",
		TrustedRecipients: []string{"acme.example", "acme2.example"},
	}

	capture := func(fn func()) string {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		defer slog.SetDefault(prev)
		fn()
		return buf.String()
	}

	trusted := capture(func() { logDetailPolicy(cfg) })
	if !strings.Contains(trusted, "acme.example") || !strings.Contains(trusted, "acme2.example") {
		t.Errorf("лог доверенного режима = %q, want оба домена из GOTCHA_TRUSTED_RECIPIENTS", trusted)
	}

	open := capture(func() {
		logDetailPolicy(Config{BaseURL: cfg.BaseURL, TrustedRecipients: cfg.TrustedRecipients, ExternalChannelDetails: true})
	})
	if !strings.Contains(open, "acme.example") || !strings.Contains(open, "acme2.example") {
		t.Errorf("лог режима EXTERNAL_CHANNEL_DETAILS_ENABLED=true = %q, want тот же список — флаг не должен прятать его от оператора", open)
	}
}

// TestSetupLoggingAcceptsKnownLevels: каждое распознанное сочетание
// level/format проходит без ошибки и не паникует. cfg.LogLevel/cfg.LogFormat
// приходят из config.go уже триммленными и в нижнем регистре — здесь
// проверяются ровно те значения, что setupLogging реально получает.
func TestSetupLoggingAcceptsKnownLevels(t *testing.T) {
	defer setupLogging("", "") // не оставлять хендлер последней итерации глобальным
	for _, level := range []string{"", "debug", "info", "warn", "warning", "error"} {
		for _, format := range []string{"", "json", "text"} {
			if err := setupLogging(level, format); err != nil {
				t.Errorf("setupLogging(%q, %q): %v", level, format, err)
			}
		}
	}
}

// TestSetupLoggingRejectsUnknownLevel — RA-контракт (задача 5, E3): нераспознанный
// GOTCHA_LOGGING_LEVEL обязан ронять старт, а не тихо откатываться на Info.
// Раньше `LOG_LEVEL=trace` во время инцидента давал молчаливый Info без
// единой диагностики, и оператор отлаживал логгер вместо инцидента.
func TestSetupLoggingRejectsUnknownLevel(t *testing.T) {
	if err := setupLogging("trace", "text"); err == nil {
		t.Error(`setupLogging("trace", "text") must fail, got nil error`)
	}
}

// TestSetupLoggingRejectsUnknownFormat — тот же контракт для GOTCHA_LOGGING_FORMAT:
// раньше нераспознанный формат тихо откатывался на text.
func TestSetupLoggingRejectsUnknownFormat(t *testing.T) {
	if err := setupLogging("info", "nonsense"); err == nil {
		t.Error(`setupLogging("info", "nonsense") must fail, got nil error`)
	}
}

// TestSetupLoggingUsesValidateLogging — K5-1: setupLogging не заводит свою
// копию таблицы допустимых level/format, а вызывает validateLogging
// (cmd/gotcha/config.go) первой строкой — ту же функцию, которую loadConfig
// зовёт в блоке накопления ошибок. Тест сравнивает ТЕКСТ ошибки: если бы
// setupLogging вернулась к собственному switch с def-веткой, тексты могли бы
// разойтись незаметно для остальных тестов (они лишь проверяют err != nil).
func TestSetupLoggingUsesValidateLogging(t *testing.T) {
	want := validateLogging("trace", "text")
	if want == nil {
		t.Fatal(`validateLogging("trace", "text") = nil, want error`)
	}
	got := setupLogging("trace", "text")
	if got == nil || got.Error() != want.Error() {
		t.Errorf("setupLogging(%q, %q) = %v, want same error as validateLogging: %v", "trace", "text", got, want)
	}
}

// TestSetupLoggingWarningAliasSetsWarnLevel — "warning" (алиас, документирован
// в обеих локалях) обязан выставлять именно slog.LevelWarn: Info-сообщения
// после него отфильтрованы, Warn — проходят.
func TestSetupLoggingWarningAliasSetsWarnLevel(t *testing.T) {
	defer setupLogging("", "")
	if err := setupLogging("warning", "text"); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	h := slog.Default().Handler()
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error(`level "warning" must disable Info logs`)
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error(`level "warning" must enable Warn logs`)
	}
}

// TestAutoMaxBufferBytesSafeUnderHeapCeiling: находка P0-1 — дефолтный
// docker-compose.yml (mem_limit 1g, GOTCHA_MAX_WRITER_BUFFER_BYTES не задан) даёт
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
// GOTCHA_MAX_WRITER_BUFFER_BYTES обязан побеждать авто-дефолт в обе стороны — и
// когда оператор просит больше, и когда меньше того, что вывел бы авто-режим.
func TestEffectiveMaxBufferBytesRespectsExplicitOverride(t *testing.T) {
	const heapCeiling = 800 << 20
	const explicit = 24 << 20 // как в docker-compose.small.yml
	if got := effectiveMaxBufferBytes(explicit, heapCeiling); got != explicit {
		t.Errorf("effectiveMaxBufferBytes(explicit=%d, heap=%d) = %d, явный GOTCHA_MAX_WRITER_BUFFER_BYTES проигнорирован",
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

// TestExportRowRetention: Store.PurgeRows чистит терминальные строки заявок
// на выгрузку по finished_at независимо от expires_at (janitor.go). Если бы
// retention был жёстко зафиксирован на 30 сутках, оператор, поднявший
// GOTCHA_EXPORT_RETENTION_HOURS выше 720 (30 суток), получил бы удаление строки
// ЖИВОЙ (ещё не истёкшей по собственному TTL) заявки раньше её срока —
// retention обязан расти вместе с TTL, а не оставаться позади него.
func TestExportRowRetention(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"дефолтный TTL (7 суток) короче минимума — минимум", 7 * 24 * time.Hour, exportMinRowRetention},
		{"TTL ровно на минимуме — минимум", exportMinRowRetention, exportMinRowRetention},
		{"TTL длиннее минимума (60 суток) — растёт вместе с TTL", 60 * 24 * time.Hour, 60 * 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exportRowRetention(c.ttl); got != c.want {
				t.Errorf("exportRowRetention(%s) = %s, want %s", c.ttl, got, c.want)
			}
		})
	}
}

// TestExportsWiringEnabled: --mode=uptime поднимает outbox (нужен
// уведомителю детектора аптайма), но НЕ строит issueSvc — воркер выгрузок,
// поднятый на одном "outbox != nil", разыменовывал бы nil *issue.Service на
// первой же заявке issues/events и ронял процесс паникой. --mode=ingest тоже
// строит issueSvc, но webHandler там не строится никогда — раздать файл
// некому, а джанитор чужой реплики топит заявку в expired раньше срока
// (P1-OPS-2). Гейт обязан требовать И режим, который отдаёт файл
// (exportModeServesFiles: web|all), И доступный каталог разом.
func TestExportsWiringEnabled(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		dirOK  bool
		enable bool
	}{
		{"web + каталог доступен — включено", "web", true, true},
		{"all + каталог доступен — включено", "all", true, true},
		{"ingest + каталог доступен — файл отдавать некому, выключено", "ingest", true, false},
		{"uptime + каталог доступен — issueSvc нет, выключено", "uptime", true, false},
		{"probe + каталог доступен — issueSvc нет, выключено", "probe", true, false},
		{"web + каталог недоступен — выключено", "web", false, false},
		{"ingest + каталог недоступен — выключено", "ingest", false, false},
		{"uptime + каталог недоступен — выключено", "uptime", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exportsWiringEnabled(c.mode, c.dirOK); got != c.enable {
				t.Errorf("exportsWiringEnabled(%q, %v) = %v, want %v", c.mode, c.dirOK, got, c.enable)
			}
		})
	}
}

// TestExportModeServesFiles: единственный источник истины для "кто отдаёт
// файл выгрузки" — этот список литералов, тот же, что гейтит построение
// webHandler в run(). ingest намеренно снаружи (P1-OPS-2): issueSvc там
// есть, маршрутов /projects/{id}/exports — нет.
func TestExportModeServesFiles(t *testing.T) {
	for _, mode := range []string{"web", "all"} {
		if !exportModeServesFiles(mode) {
			t.Errorf("exportModeServesFiles(%q) = false, want true", mode)
		}
	}
	for _, mode := range []string{"ingest", "uptime", "probe", ""} {
		if exportModeServesFiles(mode) {
			t.Errorf("exportModeServesFiles(%q) = true, want false", mode)
		}
	}
}

// TestExportDirWritable_WritableDir: каталог, реально доступный на запись
// этому процессу, проходит пробу — обычный случай (каталог создан этим же
// MkdirAll с нужным владельцем).
func TestExportDirWritable_WritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := exportDirWritable(dir); err != nil {
		t.Fatalf("exportDirWritable(%q) = %v, want nil на каталоге с правами на запись", dir, err)
	}
	// Проба не должна оставлять файл после себя — иначе каждый старт плодит
	// мусор в каталоге выгрузок.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("после exportDirWritable в каталоге осталось %d файлов, want 0: %v", len(entries), entries)
	}
}

// TestExportDirWritable_ReadOnlyDir: находка P0-OPS-1 — MkdirAll на
// каталоге, который Docker создал root:root при монтировании свежего тома,
// возвращает nil (каталог уже существует), хотя писать в него процесс не
// может. exportDirWritable обязан поймать именно этот случай мутацией
// врезки: каталог 0o555 (только чтение+исполнение, без записи) существует,
// но проба обязана вернуть ошибку.
func TestExportDirWritable_ReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root игнорирует биты записи — проба не сработает под root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod(%q, 0o555): %v", dir, err)
	}
	defer os.Chmod(dir, 0o755) // иначе t.TempDir() не сможет убрать за собой
	if err := exportDirWritable(dir); err == nil {
		t.Errorf("exportDirWritable(%q) = nil на read-only каталоге, want ошибку (P0-OPS-1: MkdirAll молчит, писать некому)", dir)
	}
}

// TestExportDirWritable_ConcurrentReplicas: несколько web-реплик за
// балансировщиком на общем томе выгрузок (internal/docs/{ru,en}/exports.md
// признаёт это поддерживаемым сценарием) стартуют и зовут exportDirWritable
// параллельно. С фиксированным именем ".probe" одна реплика удаляла файл,
// который параллельно создала и уже удалила другая, и ловила на своём
// os.Remove ENOENT — раздел выгрузок ложно выключался на проигравшей
// реплике до рестарта, хотя каталог доступен на запись. Имя пробы обязано
// быть уникальным на каждый вызов, поэтому все N конкурентных вызовов на
// одном каталоге обязаны вернуть nil.
func TestExportDirWritable_ConcurrentReplicas(t *testing.T) {
	dir := t.TempDir()
	const n = 100

	var ready sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)

	ready.Add(n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			errs[i] = exportDirWritable(dir)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			if failed <= 3 {
				t.Logf("вызов %d: %v", i, err)
			}
		}
	}
	if failed != 0 {
		t.Errorf("exportDirWritable: %d/%d конкурентных вызовов на общем каталоге вернули ошибку, want 0 (гонка на имени пробы)", failed, n)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("после %d конкурентных вызовов в каталоге осталось %d файлов, want 0: %v", n, len(entries), entries)
	}
}

// TestEnsureExportDirCreatesWithMode0700: каталог выгрузок — единственное
// место продукта, где ПДн (см. worker.go, файлы внутри уже 0600) ложатся на
// диск целым каталогом, а не файлом. Остаток P3-SEC-1: 0755 отдавал листинг
// каталога и содержимое файлов любому, кто читает с этого же хоста; сосед по
// процессу не должен иметь права даже на чтение листинга.
func TestEnsureExportDirCreatesWithMode0700(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "exports")

	if err := ensureExportDir(dir); err != nil {
		t.Fatalf("ensureExportDir(%q): %v", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", dir, err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("режим каталога выгрузок = %o, want 0700", mode)
	}
}

// TestWaitGroupWithTimeoutReturnsTrueWhenGoroutinesFinish (P2-OPS-5):
// воркер/джанитор выгрузок, отпущенные отменой ctx, обязаны дать drain()
// увидеть их завершение — a не всегда упираться в таймаут окна, иначе
// каждый деплой ждал бы полные 5с зря.
func TestWaitGroupWithTimeoutReturnsTrueWhenGoroutinesFinish(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
	}()

	start := time.Now()
	ok := waitGroupWithTimeout(&wg, time.Second)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("waitGroupWithTimeout = false, хотя горутина завершилась в срок")
	}
	if elapsed >= time.Second {
		t.Errorf("waitGroupWithTimeout дождался всего окна (%s) вместо возврата сразу после Done()", elapsed)
	}
}

// TestWaitGroupWithTimeoutReturnsFalseWithoutBlockingPastWindow (P2-OPS-5):
// зависшая горутина (не отвечает на отмену ctx) не имеет права держать
// drain() дольше окна — это ровно тот сценарий, ради которого в main.go
// стоит select с time.After, а не голый wg.Wait(). Мутация — заменить тело
// на голый wg.Wait() (без select/timeout) — обязана уронить этот тест
// таймаутом самого теста (goroutine leak detector) или зависанием.
func TestWaitGroupWithTimeoutReturnsFalseWithoutBlockingPastWindow(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	go func() {
		defer wg.Done()
		<-release
	}()
	t.Cleanup(func() { close(release) })

	start := time.Now()
	ok := waitGroupWithTimeout(&wg, 30*time.Millisecond)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("waitGroupWithTimeout = true, хотя горутина не завершилась")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("waitGroupWithTimeout ждал %s — окно (30ms) не ограничило ожидание", elapsed)
	}
}

// fakeWriterStats — писатель, рассказывающий о себе три числа (см. writerStats).
type fakeWriterStats struct{ buffered, dropped, failures int64 }

func (f *fakeWriterStats) Buffered() int64       { return f.buffered }
func (f *fakeWriterStats) Dropped() int64        { return f.dropped }
func (f *fakeWriterStats) InsertFailures() int64 { return f.failures }

// TestRegisterWriterMetricsPublishesAllThree: разбор «часть событий не
// доезжает» опирается на все три числа сразу — глубина буфера показывает,
// принимает ли хранилище, отказы вставки говорят почему, потери означают, что
// данные уже не вернуть. Потерять при регистрации любое из трёх — потерять
// половину ответа, поэтому проверяются имя, тип и метка каждой метрики.
func TestRegisterWriterMetricsPublishesAllThree(t *testing.T) {
	var r selfmetrics.Registry
	registerWriterMetrics(&r, "event", &fakeWriterStats{buffered: 7, dropped: 3, failures: 11})

	got := r.Gather()
	for _, want := range []string{
		"# TYPE gotcha_writer_buffered_rows gauge",
		"gotcha_writer_buffered_rows{writer=\"event\"} 7",
		"# TYPE gotcha_writer_dropped_rows_total counter",
		"gotcha_writer_dropped_rows_total{writer=\"event\"} 3",
		"# TYPE gotcha_writer_insert_failures_total counter",
		"gotcha_writer_insert_failures_total{writer=\"event\"} 11",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("экспозиция не содержит %q:\n%s", want, got)
		}
	}
}

// TestRegisterWriterMetricsSeparatesWritersByLabel: метка writer= — то, ради
// чего регистрация вынесена в общую функцию. Общее имя без разделяющей метки
// склеило бы пятерых писателей в одну строку, и «теряет спаны» стало бы
// неотличимо от «теряет логи».
func TestRegisterWriterMetricsSeparatesWritersByLabel(t *testing.T) {
	var r selfmetrics.Registry
	registerWriterMetrics(&r, "span", &fakeWriterStats{buffered: 1})
	registerWriterMetrics(&r, "log", &fakeWriterStats{buffered: 2})

	got := r.Gather()
	if !strings.Contains(got, "gotcha_writer_buffered_rows{writer=\"span\"} 1") ||
		!strings.Contains(got, "gotcha_writer_buffered_rows{writer=\"log\"} 2") {
		t.Errorf("писатели не разделены меткой writer=:\n%s", got)
	}
}

// TestRegisterWriterMetricsReadsValuesLazily: значения обязаны браться на
// каждый скрап, а не сниматься один раз при регистрации — снимок на старте
// показывал бы вечные нули, то есть «всё хорошо» ровно в тот момент, когда
// буфер растёт.
func TestRegisterWriterMetricsReadsValuesLazily(t *testing.T) {
	var r selfmetrics.Registry
	w := &fakeWriterStats{}
	registerWriterMetrics(&r, "metric", w)

	w.buffered = 42
	if got := r.Gather(); !strings.Contains(got, "gotcha_writer_buffered_rows{writer=\"metric\"} 42") {
		t.Errorf("значение снято на регистрации, а не на скрапе:\n%s", got)
	}
}

// TestCommonServicesEnabled: единственный источник истины для тройки режимов,
// где run() строит общие сервисы. Расхождение этого списка с проводкой уже
// давало панику при --mode=uptime (nil issueSvc), поэтому список проверяется
// целиком, включая режимы, которых в нём быть не должно.
func TestCommonServicesEnabled(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"ingest", true},
		{"web", true},
		{"all", true},
		{"uptime", false},
		{"probe", false},
		{"", false},
	} {
		if got := commonServicesEnabled(tc.mode); got != tc.want {
			t.Errorf("commonServicesEnabled(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestDrainIngestSignalsWaitsForFinalFlush (I1, аудит перед 1.0, K7-5/K7-6):
// раньше `go ingestSignals.Run(ctx)` в run() не был обёрнут ничем — drain()
// шёл к pg.Close() не дожидаясь горутины, и финальный Flush (см.
// internal/ingestsignal.Recorder.Run) гонялся с закрытием пула. Фикс —
// drainIngestSignals, тот же приём, что exportWorkersWG/waitGroupWithTimeout.
//
// Чтобы проверка не зависела от везения планировщика, пул исчерпывается ДО
// отмены ctx: Bump внутри финального Flush гарантированно блокируется в
// Acquire, пока тест не отпустит соединения. Мутация — заменить тело
// drainIngestSignals на no-op (не ждать вовсе) — обязана уронить первую
// проверку: без ожидания функция вернётся немедленно, не дав Run дойти до
// Flush.
func TestDrainIngestSignalsWaitsForFinalFlush(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('ingest-signals-drain', 'Drain Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'ingest-signals-drain', 'Drain Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	st := ingestsignal.NewStore(pool)
	rec := ingestsignal.NewRecorder(st)
	rec.FlushEvery = time.Hour // тик не должен успеть сработать сам за время теста
	rec.Touch(projectID, ingestsignal.KindKeyInvalid)

	// Исчерпать пул: Bump внутри финального Flush не сможет получить
	// соединение, пока мы не отпустим все Acquire ниже.
	maxConns := int(pool.Config().MaxConns)
	held := make([]*pgxpool.Conn, 0, maxConns)
	for i := 0; i < maxConns; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, c)
	}
	// Release гарантированно один раз и даже при t.Fatal ниже: без этого
	// зависший Acquire не даёт pool.Close() в t.Cleanup(MigratedPG) вернуться
	// (Close ждёт возврата ВСЕХ выданных соединений), и тест виснет вместо
	// того, чтобы упасть.
	release := sync.OnceFunc(func() {
		for _, c := range held {
			c.Release()
		}
	})
	t.Cleanup(release)

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec.Run(runCtx)
	}()
	cancel() // как в drain(): ctx уже отменён к этому моменту, Run уходит в финальный Flush

	drainDone := make(chan struct{})
	go func() {
		drainIngestSignals(&wg)
		close(drainDone)
	}()

	// Соединения заняты нами — drainIngestSignals обязан ещё ждать.
	select {
	case <-drainDone:
		t.Fatal("drainIngestSignals вернулся, пока пул исчерпан и Flush не мог завершиться — ожидания нет")
	case <-time.After(150 * time.Millisecond):
	}

	release()

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drainIngestSignals не вернулся после освобождения соединений")
	}

	got, err := st.ForProject(ctx, projectID)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ingestsignal.KindKeyInvalid || got[0].Hits != 1 {
		t.Fatalf("сигналов = %+v, want ровно [key_invalid hits=1] — drainIngestSignals обязан был дождаться финального Flush", got)
	}
}
