package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestExportRowRetention: Store.PurgeRows чистит терминальные строки заявок
// на выгрузку по finished_at независимо от expires_at (janitor.go). Если бы
// retention был жёстко зафиксирован на 30 сутках, оператор, поднявший
// GOTCHA_EXPORT_TTL_HOURS выше 720 (30 суток), получил бы удаление строки
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
