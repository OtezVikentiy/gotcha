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

func TestEvaluatorsDisabledWarningNamesAllSixCycles(t *testing.T) {
	for _, want := range []string{"metric", "profile", "host", "slo", "escalation"} {
		if !strings.Contains(evaluatorsDisabledWarning, want) {
			t.Errorf("evaluatorsDisabledWarning не упоминает %q: %s", want, evaluatorsDisabledWarning)
		}
	}
	// В тексте предупреждения и в UI trace-пакет называется "performance"/
	// "regression", не "trace".
	if !strings.Contains(evaluatorsDisabledWarning, "regression") {
		t.Errorf("evaluatorsDisabledWarning не упоминает регрессии производительности (trace.Evaluator): %s",
			evaluatorsDisabledWarning)
	}
}

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
	logDetailPolicy(cfg)
	logDetailPolicy(Config{BaseURL: cfg.BaseURL, ExternalChannelDetails: true})
}

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

func TestSetupLoggingRejectsUnknownLevel(t *testing.T) {
	if err := setupLogging("trace", "text"); err == nil {
		t.Error(`setupLogging("trace", "text") must fail, got nil error`)
	}
}

func TestSetupLoggingRejectsUnknownFormat(t *testing.T) {
	if err := setupLogging("info", "nonsense"); err == nil {
		t.Error(`setupLogging("info", "nonsense") must fail, got nil error`)
	}
}

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

func TestAutoMaxBufferBytesNoLimitFallsBackToPackageDefault(t *testing.T) {
	for _, heapCeiling := range []int64{0, -1} {
		if got := autoMaxBufferBytes(heapCeiling); got != 0 {
			t.Errorf("autoMaxBufferBytes(%d) = %d, хочу 0 (сигнал «оставь flat-дефолт пакета»)", heapCeiling, got)
		}
	}
}

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

func TestCloseBoundedReturnsTrueWhenCloseFnFinishes(t *testing.T) {
	start := time.Now()
	ok := closeBounded(func() { time.Sleep(5 * time.Millisecond) }, time.Second)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("closeBounded = false, хотя closeFn завершился в срок")
	}
	if elapsed >= time.Second {
		t.Errorf("closeBounded дождался всего окна (%s) вместо возврата сразу после завершения closeFn", elapsed)
	}
}

func TestCloseBoundedReturnsFalseWithoutBlockingPastWindow(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	ok := closeBounded(func() { <-release }, 30*time.Millisecond)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("closeBounded = true, хотя closeFn не завершился")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("closeBounded ждал %s — окно (30ms) не ограничило ожидание", elapsed)
	}
}

func TestDrainParallelWaitsForEveryBranch(t *testing.T) {
	const n = 3
	release := make([]chan struct{}, n)
	for i := range release {
		release[i] = make(chan struct{})
	}

	finished := make(chan struct{})
	go func() {
		drainParallel(
			func() { <-release[0] },
			func() { <-release[1] },
			func() { <-release[2] },
		)
		close(finished)
	}()

	close(release[0])
	close(release[1])
	select {
	case <-finished:
		t.Fatal("drainParallel вернулась, не дождавшись всех веток")
	case <-time.After(100 * time.Millisecond):
	}

	close(release[2])
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("drainParallel не вернулась после завершения всех веток")
	}
}

func TestIngestSignalsFinalFlushFitsDrainWindow(t *testing.T) {
	if ingestsignal.FinalFlushTimeout >= ingestSignalsDrainWindow {
		t.Fatalf("ingestsignal.FinalFlushTimeout (%s) >= ingestSignalsDrainWindow (%s): "+
			"финальный флаш не успевает уложиться в окно, которым drain() ждёт Recorder.Run",
			ingestsignal.FinalFlushTimeout, ingestSignalsDrainWindow)
	}
}

type fakeWriterStats struct{ buffered, dropped, failures int64 }

func (f *fakeWriterStats) Buffered() int64       { return f.buffered }
func (f *fakeWriterStats) Dropped() int64        { return f.dropped }
func (f *fakeWriterStats) InsertFailures() int64 { return f.failures }

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

func TestRegisterWriterMetricsReadsValuesLazily(t *testing.T) {
	var r selfmetrics.Registry
	w := &fakeWriterStats{}
	registerWriterMetrics(&r, "metric", w)

	w.buffered = 42
	if got := r.Gather(); !strings.Contains(got, "gotcha_writer_buffered_rows{writer=\"metric\"} 42") {
		t.Errorf("значение снято на регистрации, а не на скрапе:\n%s", got)
	}
}

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
	// sync.OnceFunc: без гарантии единственного Release зависший Acquire не
	// даёт pool.Close() вернуться, и тест виснет вместо падения.
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
	cancel() // как в drain(): ctx уже отменён, Run уходит в финальный Flush

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
