package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/memlimit"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	// Часовые пояса — в самом бинаре. Базовый образ (alpine) tzdata не несёт, и
	// без этого time.LoadLocation падает на любом поясе, кроме UTC: форма окон
	// обслуживания предлагает Europe/Moscow и соседей, а сохранить их было
	// нельзя. Хуже того, окно с непонятным поясом роняло проверку обслуживания
	// в детекторе, и инцидент не открывался вовсе.
	//
	// Пакет, а не apk add: так продукт не зависит от базового образа — бинарь,
	// собранный и запущенный где угодно, ведёт себя одинаково. Цена — около
	// 450 КБ.
	_ "time/tzdata"
)

func main() {
	// Проверка состояния — до всего остального: она не поднимает ни соединений,
	// ни сервера, только спрашивает уже работающий процесс.
	if url, ok := healthcheckRequested(os.Args[1:], os.Getenv); ok {
		os.Exit(runHealthcheck(url))
	}
	if err := run(); err != nil {
		slog.Error("gotcha failed", "error", err)
		os.Exit(1)
	}
}

// versionRequested — true, если среди аргументов есть флаг версии.
func versionRequested(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "version" {
			return true
		}
	}
	return false
}

// deriveCookieKey выводит из мастер-секрета отдельный подключ для HMAC-подписи
// oauth-flow cookie (доменное разделение, Info20): HMAC-SHA256(master, label).
// Детерминирован (переживает рестарт), не совпадает с ключом at-rest-шифрования
// SSO (sha256(master) в org). Пустой мастер → пустой ключ: web-слой сам
// подставит дефолт для стендов (см. oauthflow.go secret()).
func deriveCookieKey(master string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte("gotcha:oauth-cookie-mac:v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// setupLogging настраивает глобальный slog по GOTCHA_LOGGING_LEVEL/GOTCHA_LOGGING_FORMAT.
// Пустое значение — прежнее поведение (текст, уровень Info); json — для
// Loki/ELK, debug — чтобы поднять детализацию во время инцидента без
// пересборки, warning — принятый в проде псевдоним warn (документирован в
// обеих локалях). Нераспознанное непустое значение — отказ старта, а не
// тихий откат к дефолту: раньше LOG_LEVEL=trace во время инцидента давал
// молчаливый Info без единой диагностики, и оператор тратил цикл на отладку
// самого логгера вместо инцидента — прямое нарушение собственной конвенции
// продукта («невалидное значение любой переменной — отказ старта», см.
// internal/docs/{ru,en}/configuration.md). level/format приходят из
// cfg.LogLevel/cfg.LogFormat уже триммленными и в нижнем регистре
// (cmd/gotcha/config.go), здесь их разбирать повторно не нужно.
func setupLogging(level, format string) error {
	// validateLogging (cmd/gotcha/config.go) — та же таблица значений, что
	// loadConfig проверяет на старте (K5-1): вызывается здесь первой строкой,
	// дальше только строится хендлер по уже провалидированным level/format —
	// вторую таблицу допустимых значений заводить незачем.
	if err := validateLogging(level, format); err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: logLevels[level]}
	var h slog.Handler
	switch format {
	case "", "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

// writerStats — то, что каждый буферизованный писатель рассказывает о себе.
// Один интерфейс на пятерых (события, спаны, метрики, профили, аптайм) —
// добавление шестого не потребует трогать регистрацию.
type writerStats interface {
	Buffered() int64
	Dropped() int64
	InsertFailures() int64
}

// registerWriterMetrics публикует три счётчика писателя под меткой writer=.
// Именно этих трёх не хватало при разборе «часть событий не доезжает»: глубина
// буфера показывает, принимает ли хранилище, отказы вставки — почему, а потери
// говорят, что данные уже НЕ вернуть.
func registerWriterMetrics(r *selfmetrics.Registry, name string, w writerStats) {
	lbl := map[string]string{"writer": name}
	r.AddInt(selfmetrics.Gauge, "gotcha_writer_buffered_rows",
		"Rows buffered in memory, waiting to be written to ClickHouse.", lbl, w.Buffered)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_dropped_rows_total",
		"Rows dropped because the buffer overflowed; these are lost for good.", lbl, w.Dropped)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_insert_failures_total",
		"Failed batch inserts. The batch is retried, so this is not data loss by itself.", lbl, w.InsertFailures)
}

// autoBufferSafeShare — доля потолка кучи, отдаваемая СУММЕ всех писательских
// буферов, когда per-writer-потолок выводится автоматически (см.
// effectiveMaxBufferBytes). Остаток — запас на всё, что не буфер: HTTP-приём,
// разбор JSON, клиент PostgreSQL, сам рантайм поверх GC-паузы.
const autoBufferSafeShare = 0.6

// autoBufferCapUnits — на сколько «единиц потолка» делится autoBufferSafeShare
// потолка кучи. Писателей пять (event, SpanWriter, metric, profile, log), но
// SpanWriter применяет ОДИН per-writer-потолок к ДВУМ независимым буферам
// (txBuf и spanBuf, см. SpanWriter.SetMaxBufferBytes) — в худшем случае оба
// заполнены доверху одновременно, значит SpanWriter считается за 2 единицы:
// event(1) + SpanWriter(2) + metric(1) + profile(1) + log(1) = 6.
const autoBufferCapUnits = 6

// autoMaxBufferBytes выводит безопасный per-writer байтовый потолок буфера из
// обнаруженного потолка кучи (heapLimitBytes — то, что вернул applyMemoryLimit,
// 0 если лимит не обнаружен или GOMEMLIMIT не удалось вывести). Используется,
// только когда оператор не задал GOTCHA_MAX_WRITER_BUFFER_BYTES явно: тогда каждый из
// шести буферов-«единиц» брал бы flat defaultMaxBufBytes=256 МиБ пакета-писателя
// (1.5 ГиБ суммарно) — больше heap-потолка на дефолтном docker-compose.yml
// (mem_limit 1g → потолок кучи 819 МиБ), то есть дефолтная поставка могла
// схватить OOM ядра при простое ClickHouse. Возвращает 0, если heapLimitBytes
// <= 0 — вызывающий в этом случае оставляет прежний flat-дефолт пакета
// (SetMaxBufferBytes(0) — no-op, регресса нет).
func autoMaxBufferBytes(heapLimitBytes int64) int64 {
	if heapLimitBytes <= 0 {
		return 0
	}
	return int64(float64(heapLimitBytes) * autoBufferSafeShare / autoBufferCapUnits)
}

// effectiveMaxBufferBytes решает, какой per-writer байтовый потолок ставить
// каждому писателю. Явный GOTCHA_MAX_WRITER_BUFFER_BYTES (cfgMaxBufferBytes
// != 0) всегда побеждает — оператор, написавший число руками, имел в виду
// именно его. Иначе — авто-дефолт от обнаруженного потолка кучи
// (autoMaxBufferBytes); если и его вывести не из чего — 0, что для
// SetMaxBufferBytes писателя означает «оставить дефолт пакета» (256 МиБ).
func effectiveMaxBufferBytes(cfgMaxBufferBytes, heapLimitBytes int64) int64 {
	if cfgMaxBufferBytes != 0 {
		return cfgMaxBufferBytes
	}
	return autoMaxBufferBytes(heapLimitBytes)
}

// exportMinRowRetention — минимальный срок хранения строки завершённой
// заявки на выгрузку (export.Janitor.RowRetention), не завязанный на
// GOTCHA_EXPORT_RETENTION_HOURS: статус "истекла" должен успеть побыть видимым на
// странице, а не исчезнуть в тот же тик, что и файл.
const exportMinRowRetention = 30 * 24 * time.Hour

// exportRowRetention возвращает срок хранения истории заявок на выгрузку —
// не короче TTL самого файла (ttl): Store.PurgeRows чистит терминальные
// строки по finished_at независимо от expires_at, и если бы retention был
// зафиксирован на exportMinRowRetention, при GOTCHA_EXPORT_RETENTION_HOURS больше
// 30 суток он снёс бы строку ЖИВОЙ (ещё не истёкшей по собственному TTL)
// заявки раньше её собственного срока.
func exportRowRetention(ttl time.Duration) time.Duration {
	if ttl > exportMinRowRetention {
		return ttl
	}
	return exportMinRowRetention
}

// waitGroupWithTimeout ждёт wg не дольше timeout и сообщает, успел ли он
// завершиться (P2-OPS-5): вынесено из drain() в run() отдельной функцией,
// чтобы само ограниченное ожидание — то, что окно НЕ растягивается на всю
// работающую горутину и не виснет, если горутина никогда не завершится, —
// было проверяемо юнит-тестом без поднятия PG/ClickHouse/HTTP, которые
// требует run() целиком.
func waitGroupWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ingestSignalsDrainWindow — сколько drainIngestSignals ждёт Recorder.Run.
// Названо константой (а не литералом на месте вызова), чтобы
// TestIngestSignalsFinalFlushFitsDrainWindow мог сверить её с
// ingestsignal.FinalFlushTimeout напрямую — копию с истиной, а не копию с
// копией (L, раунд правок по ревью финревью волны 1 аудита перед 1.0):
// Recorder.Run возвращается (и закрывает wg) только ПОСЛЕ финального Flush,
// поэтому это окно обязано быть строго БОЛЬШЕ FinalFlushTimeout — иначе при
// медленной PG внешнее ожидание гарантированно проигрывало бы гонку
// собственному флашу ещё до того, как Run успеет вернуться.
const ingestSignalsDrainWindow = 5 * time.Second

// drainIngestSignals ждёт горутину Recorder.Run сигналов приёма (K7-5/K7-6)
// перед тем, как drain() продолжит к pg.Close(): Run уже увидел <-ctx.Done()
// и делает финальный Flush, но без этого ожидания он гонится с закрытием
// пула, и накопленное с последнего тика теряется молча. Тот же приём и то же
// окно, что у exportWorkersWG (см. P2-OPS-5) — вынесено отдельной функцией,
// чтобы саму гарантию ожидания можно было проверить тестом без запуска run().
func drainIngestSignals(wg *sync.WaitGroup) {
	if !waitGroupWithTimeout(wg, ingestSignalsDrainWindow) {
		slog.Warn("ingest signals recorder did not stop within the shutdown window")
	}
}

// closeBounded запускает closeFn в горутине и сообщает, успела ли она
// завершиться за timeout (I3, финревью волны 1 аудита перед 1.0): нужен
// закрывашкам без собственного ctx/дедлайна — сейчас единственная,
// uptime.Runner.Close — чтобы зависшая остановка не растягивала drain()
// дольше её бюджета. По превышении closeFn продолжает работать в
// горутине-сироте (утечка до её собственного завершения), а drain() идёт
// дальше — тот же компромисс, что уже принят для pipeline.Close/
// waitGroupWithTimeout: не потерять весь процесс по SIGKILL дороже, чем
// изредка не досчитаться самой последней записи опоздавшего писателя.
func closeBounded(closeFn func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// drainParallel запускает каждую из branches в своей горутине и не
// возвращается, пока ВСЕ не завершились (I3, финревью волны 1 аудита перед
// 1.0) — вынесено из drain() отдельной функцией, чтобы само это свойство
// (ожидание, а не только запуск) было проверяемо юнит-тестом без поднятия
// run() целиком. drain() строит branches из независимых цепочек писателей —
// см. её докблок про то, какие ветки можно параллелить, а какие нет.
func drainParallel(branches ...func()) {
	var dwg sync.WaitGroup
	dwg.Add(len(branches))
	for _, b := range branches {
		go func() {
			defer dwg.Done()
			b()
		}()
	}
	dwg.Wait()
}

// commonServicesEnabled — режимы, в которых run() строит общие сервисы
// (orgSvc/issueSvc/alertSvc/emailSender/outbox, см. блок "Общие сервисы").
// Единственный источник истины для этой тройки: список литералов существует
// в коде ровно один раз, а не дублируется текстом по разным местам — именно
// такое молчаливое дублирование ("тот же гейт ingest|web|all" в комментарии,
// не подкреплённое общим кодом) и породило панику при --mode=uptime, которую
// чинит exportsWiringEnabled ниже.
func commonServicesEnabled(mode string) bool {
	return mode == "ingest" || mode == "web" || mode == "all"
}

// exportModeServesFiles — режимы, где сам процесс отдаёт скачивание файла
// выгрузки (webHandler строится только тут, см. блок "if cfg.Mode == "web"
// || cfg.Mode == "all"" ниже). Единственный источник истины для этой пары:
// exportsWiringEnabled и проба каталога (run()) обязаны смотреть на один и
// тот же список литералов, а не дублировать его текстом — такое
// дублирование ("тот же гейт ingest|web|all" в комментарии, не подкреплённое
// общим кодом) уже породило панику при --mode=uptime (см. докблок
// exportsWiringEnabled) и было исходной причиной P1-OPS-2 (воркер/джанитор
// выгрузок поднимались в --mode=ingest, где раздать файл некому).
func exportModeServesFiles(mode string) bool {
	return mode == "web" || mode == "all"
}

// exportsWiringEnabled решает, поднимать ли обработчик выгрузок ошибок/
// событий (E1) в этом процессе — каталог (dirOK, включая пробу на запись,
// см. exportDirWritable) И режим, который отдаёт файл (exportModeServesFiles).
// Оба обязаны выполниться разом. Раньше вторым условием был
// commonServicesEnabled (ingest|web|all): issueSvc там действительно
// строится, но воркер и джанитор — не issueSvc ради issueSvc, а обслуга
// ОЧЕРЕДИ webHandler'а; в --mode=ingest webHandler не строится никогда
// (exportModeServesFiles ниже), и заявка, собранная там воркером, лежит на
// диске реплики без единого маршрута /projects/{id}/exports — а джанитор
// СВОЕЙ реплики, увидев там ENOENT, помечает чужую заявку expired и теряет
// файл до PurgeRows (30 суток). --mode=uptime поднимает outbox (нужен
// уведомителю детектора аптайма), но issueSvc там остаётся nil, и
// export.NewIssueSource/NewEventSource, получив nil *issue.Service, всё
// равно возвращают НЕ-nil интерфейсы — воркер выгрузок стартовал бы и на
// первой же заявке issues/events упал паникой (issueSource.Stream
// разыменовывает nil *issue.Service); exportModeServesFiles исключает и этот
// режим тем же условием, без отдельной проверки issueSvc.
func exportsWiringEnabled(mode string, dirOK bool) bool {
	return dirOK && exportModeServesFiles(mode)
}

// exportDirMode — права каталога выгрузок при создании. 0700, а не 0755
// (остаток P3-SEC-1): каталог выгрузок — единственное место продукта, где
// ПДн (user_email/user_ip и т.п., см. worker.go) ложатся на диск целым
// каталогом, а не только файлом — файлы внутри уже 0600, но листинг
// каталога 0755 сам по себе был читаем любому пользователю этого хоста.
//
// os.MkdirAll НЕ меняет режим уже существующего каталога (тот же нюанс, из-за
// которого exportDirWritable ниже вообще понадобился): на инсталляциях, где
// каталог создан ДО этого патча или кем-то другим (например Docker при
// монтировании тома, см. докблок exportDirWritable), права остаются
// прежними, и их лечит оператор вручную — см. configuration.md обеих локалей.
const exportDirMode = 0o700

// ensureExportDir создаёт каталог выгрузок с exportDirMode. Вынесено из
// run() отдельной функцией ради теста: run() слишком велик, чтобы гонять его
// целиком ради проверки одной строки прав.
func ensureExportDir(dir string) error {
	return os.MkdirAll(dir, exportDirMode)
}

// exportDirWritable проверяет, что каталог выгрузок действительно доступен
// НА ЗАПИСЬ этому процессу. os.MkdirAll на каталоге, который уже существует,
// возвращает nil независимо от его владельца и прав (P0-OPS-1): штатный
// docker-compose монтирует свежий именованный том в каталог, которого нет в
// образе, точку монтирования создаёт сам Docker — root:root 0755, финальный
// слой образа работает под uid 10001 (см. Dockerfile) — MkdirAll молча
// проходит, exportDirOK становится true, раздел включается целиком, и КАЖДАЯ
// заявка падает на OpenFile("permission denied"), уходя в failed с письмом
// автору. Проба создаёт и сразу удаляет временный файл — то же самое, что
// воркер делает на каждой реальной заявке (writer.go, .part-файл), только
// раньше и без заявки в очереди. Имя пробы уникально на каждый вызов
// (os.CreateTemp, шаблон ".probe-*"): несколько web-реплик за балансировщиком
// (internal/docs/{ru,en}/exports.md признаёт это поддерживаемым сценарием)
// стартуют параллельно на общем томе, и с фиксированным именем ".probe" один
// os.Remove забирал файл, который параллельно создала и уже удалила другая
// реплика, — вторая ловила ENOENT на своём же os.Remove и ложно выключала
// раздел выгрузок до рестарта на исправном каталоге.
func exportDirWritable(dir string) error {
	// os.CreateTemp создаёт файл с правами 0600 (до umask) сам — отдельный
	// Chmod не нужен.
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	probe := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(probe)
		return err
	}
	return os.Remove(probe)
}

// applyMemoryLimit приводит потолок кучи к лимиту контейнера и сообщает
// результат. Отсутствие лимита — не ошибка: продукт не выдумывает потолок за
// оператора, но и не молчит об этом.
func applyMemoryLimit() int64 {
	limit, err := memlimit.Apply()
	switch {
	case errors.Is(err, memlimit.ErrNoLimit):
		slog.Info("no container memory limit found; heap ceiling not set " +
			"(buffers grow until the host runs out — set mem_limit or GOMEMLIMIT)")
		return 0
	case err != nil:
		slog.Warn("cannot derive heap ceiling from container limit", "error", err)
		return 0
	}
	slog.Info("heap ceiling set", "bytes", limit)
	return limit
}

func run() error {
	if versionRequested(os.Args[1:]) {
		fmt.Println("gotcha", version.String())
		return nil
	}
	cfg, err := loadConfigChecked(os.Getenv, os.Environ, os.Args[1:])
	if err != nil {
		return err
	}
	if err := setupLogging(cfg.LogLevel, cfg.LogFormat); err != nil {
		return err
	}
	// --migrate-force: снять dirty-флаг и выйти, не поднимая ни одного
	// компонента. Раньше текст ошибки советовал `migrate force` — бинарь,
	// которого в образе нет (находка №41).
	if cfg.MigrateForcePG >= 0 || cfg.MigrateForceCH >= 0 {
		if cfg.MigrateForcePG >= 0 {
			err = db.ForcePG(cfg.PostgresDSN, uint(cfg.MigrateForcePG))
		} else {
			err = db.ForceCH(cfg.ClickHouseDSN, uint(cfg.MigrateForceCH))
		}
		if err != nil {
			return err
		}
		slog.Info("dirty flag cleared; force does not finish the migration — "+
			"verify the schema before restarting (see /docs/upgrade)",
			"pg_version", cfg.MigrateForcePG, "ch_version", cfg.MigrateForceCH)
		return nil
	}
	// Потолок кучи по лимиту контейнера. Go-рантайм лимит cgroup не читает, а у
	// gotcha буферы растут по замыслу, дожидаясь возвращения хранилища: без
	// потолка первым срабатывает OOM-killer ядра, и теряется всё буферизованное,
	// а не избыток. До этой правки защита существовала только как GOMEMLIMIT в
	// small-оверлее compose.
	memLimitBytes := applyMemoryLimit()
	if cfg.SecretKey == devSecretKey {
		slog.Warn("GOTCHA_SECRET_KEY is not set — using insecure dev default (fine for localhost only)")
	}
	// SEC-M3: сессионная cookie без Secure на не-loopback HTTP уходит открытым
	// текстом (сниффинг/replay). Для продукта мониторинга дефолт должен толкать к TLS.
	if !isLocalBaseURL(cfg.BaseURL) && !strings.HasPrefix(cfg.BaseURL, "https://") {
		slog.Warn("GOTCHA_BASE_URL is non-local plain HTTP — session cookies ride unencrypted; enable TLS (https)")
	}
	// Исходящие OIDC-вызовы (discovery/JWKS/token/userinfo) — SSRF-safe по тому же
	// флагу, что webhook/uptime: приватные адреса режутся, если оператор не разрешил
	// их явно (внутренний IdP). Ставим до любого OAuth-обмена.
	oauth.SetAllowPrivateHosts(cfg.SSRFAllowPrivateOIDC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Выносная проба — отдельная ветка до всего остального: ей не нужны ни
	// Postgres, ни ClickHouse, ни HTTP-сервер (и входящих портов у неё в
	// чужом регионе может не быть вовсе). Только исходящие запросы к центру,
	// пока не придёт сигнал.
	if cfg.Mode == "probe" {
		probe := &uptime.ProbeClient{
			ServerURL:           cfg.ServerURL,
			Token:               cfg.ProbeToken,
			Concurrency:         cfg.UptimeConcurrency,
			AllowPrivateTargets: cfg.SSRFAllowPrivateUptime,
		}
		probe.Run(ctx)
		slog.Info("probe stopped")
		return nil
	}

	// Кому уйдут детали события — печатаем один раз на старте, до того как
	// заработает первый нотифаер (в режиме probe нотифаеров нет вовсе).
	logDetailPolicy(cfg)

	pg, err := db.NewPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pg.Close()

	ch, err := db.NewClickHouse(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return err
	}
	defer ch.Close()

	// Применение миграций — в migrate.go (applyMigrations): вынесено отдельной
	// функцией по той же причине, что и newRootMux/newServer в server.go —
	// TestMigrationStagesAreLogged должен вызывать ровно тот код, что
	// выполняется здесь при старте, а не собственную копию.
	if err := applyMigrations(ctx, cfg, pg, ch); err != nil {
		return err
	}

	// --migrate-only: схема применена, больше делать нечего. Отдельный
	// init-job для развёртываний с GOTCHA_AUTO_MIGRATE_ENABLED=false, где миграции
	// прогоняют один раз перед стартом реплик.
	if cfg.MigrateOnly {
		slog.Info("migrations applied, exiting (--migrate-only)")
		return nil
	}

	// Самотелеметрия. Метрики регистрируются здесь, а роут — на корневом mux в
	// newRootMux, куда deps.selfMetrics передаётся уже заполненным: реестр
	// ленив — он хранит функции и опрашивает их на каждом скрапе, поэтому
	// порядок регистрации значения не имеет.
	var selfMetrics selfmetrics.Registry
	// Граница, о которой нельзя узнать, — не граница: потолок кучи виден в
	// метриках рядом с буферами, которые в него упираются. 0 означает «потолка
	// нет» (контейнер без лимита или запуск не в контейнере).
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_memory_limit_bytes",
		"Heap ceiling in effect (GOMEMLIMIT), derived from the container limit; 0 when unlimited.",
		nil, func() int64 { return memLimitBytes })
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_build_info",
		"Build metadata; the value is always 1, the version lives in the label.",
		map[string]string{
			"version": version.String(),
			"mode":    cfg.Mode,
			// stamped=false — сборка мимо make: версия взята из исходников,
			// а не из git-тега, и сверка «задеплоено то, что думаем»
			// невозможна (находка №102).
			"stamped": strconv.FormatBool(version.Stamped()),
		},
		func() float64 { return 1 })
	// K1: промах перевода (ключа нет в запрошенной локали — fallback на
	// локаль по умолчанию, либо нет НИГДЕ — missing, страница показывает
	// сырой ключ) не падал и не переставал рендерить страницу, но и не был
	// виден никак, кроме дедуплицированного slog.Warn. Регистрация не
	// зависит от cfg.Mode: i18n.T зовётся и из web (страницы), и из notify
	// (текст уведомлений) независимо от режима процесса.
	for _, locale := range i18n.SupportedLocales() {
		for _, stage := range i18n.MissingKeyStages() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_i18n_missing_key_total",
				"Translation key lookups that missed: stage=\"fallback\" found the key in the default locale, stage=\"missing\" found it nowhere and the raw key was shown. Rendering never fails on a miss — see internal/i18n/catalog.go.",
				map[string]string{"locale": locale, "stage": string(stage)},
				func() int64 { return i18n.MissingKeyTotal(locale, stage) })
		}
	}
	// Заполнение диска (находка №40): полный список самометрик молчал про
	// место — 95% и 100% заполнения выглядели одинаково, сигнала не было
	// вовсе, только рост отброшенных вставок и провал /readyz постфактум.
	// ClickHouse отдаёт настоящее свободное/общее место на томе
	// (system.disks); PostgreSQL честно этого не может — см. docstring
	// pgUsedBytesSource в storagemetrics.go про то, почему это отдельная,
	// иначе названная метрика, а не free_bytes/total_bytes с чужим смыслом.
	// Опрос — фоновый и по таймауту (storagemetrics.go), не на каждый скрап.
	storagePollers := registerStorageMetrics(&selfMetrics, chDiskSource{conn: ch})
	go storagePollers.Run(ctx)
	pgUsedBytes := registerUsedBytesMetric(&selfMetrics, "postgres", pgUsedBytesSource{pool: pg})
	go pgUsedBytes.Run(ctx)

	// Кольцо at-rest ключей — одно на процесс, раздаётся всем сервисам, которые
	// шифруют секреты (SSO client_secret, alert-каналы, HTTP-заголовки
	// мониторов). Не строится здесь: и сборка (secretbox.NewKeyring из
	// cfg.SecretKey/SecretKeyPrev), и раздача — в wireSecretRing ниже, ПОСЛЕ
	// того как построены все три сервиса-потребителя (блоки ниже). Собраны в
	// одну функцию, а не оставлены инлайном в run(), — так обе половины
	// «проводки кольца» можно проверить тестом отдельно от bootstrap; см.
	// docstring wireSecretRing про то, какие два регресса раньше проходили
	// мимо CI незамеченными.

	// Общие сервисы нужны и ingest-у, и web-у — строим один раз на любой
	// активный режим, а не дублируем на каждый. alertSvc/emailSender/outbox
	// тоже общие: ingest использует их для срабатывания правил
	// (Evaluator/Spike) и доставки, а web — для страницы
	// /projects/{id}/alerts (правила/каналы/failed-доставки, см.
	// web.Handler.Alerts/Outbox) и синхронной отправки писем-приглашений (см.
	// web.Handler.Email в orgsettings.go).
	var orgSvc *org.Service
	var issueSvc *issue.Service
	var alertSvc *alert.Service
	var emailSender *notify.EmailSender
	var outbox *notify.Outbox
	if commonServicesEnabled(cfg.Mode) {
		orgSvc = org.NewService(pg, cfg.DefaultEventQuota)
		orgSvc.SetQuotaDefaults(cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
		// Кольцо at-rest ключей (SSO client_secret) раздаётся не здесь, а
		// централизованно в wireSecretRing ниже, после того как построены все
		// сервисы — см. её docstring про rationale (Info21) и про то, почему
		// это единственная точка раздачи, а не по вызову на конструктор.
		issueSvc = issue.NewService(pg)
		alertSvc = alert.NewService(pg)
		alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		emailSender = notify.NewEmailSender(notify.EmailConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
		})
		outbox = notify.NewOutbox(pg)
	}

	// depSuppressor — гейт зависимостей (B5, T8): один инстанс на процесс,
	// используется и детектором аптайма ниже, и оценщиками хостов/эскалации в
	// startEvaluators — кеш снапшота графа зависимостей общий на тик, а не
	// пересобирается отдельно на каждого потребителя. settleGrace — задержка
	// первого уведомления узла с задекларированным родителем (см.
	// DependencySettleSeconds), общая для обоих потребителей (uptime.Detector,
	// escalation.Scheduler).
	depSuppressor := depsuppress.NewSuppressor(pg)
	settleGrace := time.Duration(cfg.DependencySettleSeconds) * time.Second

	// Группы инцидентов (D3): один Grouper на процесс поверх того же
	// depSuppressor — DownRoot ходит по общему 5с-кешу снимка B5.
	groupStore := incidentgroup.NewStore(pg)
	grouper := &incidentgroup.Grouper{Pool: pg, Store: groupStore, Roots: depSuppressor}

	// uptimeSvc/uptimeWriter — как и orgSvc/issueSvc выше, общие для любого
	// активного режима, которому они нужны: web монтирует героя этой задачи,
	// публичный heartbeat-роут (webHandler.Uptime/UptimeWriter), даже когда
	// сам процесс не крутит Runner (--mode=web без --mode=uptime, например
	// отдельная реплика для входящих HTTP-запросов); uptime собирает поверх
	// них Runner. В --mode=all оба контура делят один ResultWriter — одна
	// очередь вставок в ClickHouse на процесс, а не по одной на контур.
	var uptimeSvc *uptime.Service
	var uptimeWriter *uptime.ResultWriter
	var uptimeDetector *uptime.Detector
	var uptimeNotifier *uptime.OutboxNotifier
	var uptimeIngestor *uptime.Ingestor
	if cfg.Mode == "web" || cfg.Mode == "uptime" || cfg.Mode == "all" {
		uptimeSvc = uptime.NewService(pg)
		// Кольцо at-rest ключей (заголовки HTTP-мониторов) раздаётся не здесь —
		// см. wireSecretRing ниже. Бэкфилл (второй эшелон, разово дошифровать
		// заголовки мониторов, сохранённых ДО включения шифрования) тоже не
		// запускается тут же — все три прохода собраны в rewrapAllSecrets
		// ниже, после того как кольцо роздано ВСЕМ сервисам.
		// Имя встроенного региона — конфигурируемое: Service.Regions предлагает
		// его в форме монитора, и оно обязано совпадать с тем, которое лизит
		// Runner ниже, иначе монитор попал бы в регион, который не проверяет
		// никто.
		uptimeSvc.LocalRegion = cfg.LocalRegion
		uptimeWriter = uptime.NewResultWriter(ch)
		registerWriterMetrics(&selfMetrics, "uptime_results", uptimeWriter)
		go uptimeWriter.Run()

		// alertSvc/outbox/emailSender are only built for ingest/web/all above —
		// uptime alone doesn't need alerting/outbox for anything else, but the
		// detector's notifier (down/up/ssl_expiring/reminder) is delivered
		// through the very same Outbox, so build them here too when uptime
		// runs on its own.
		if alertSvc == nil {
			alertSvc = alert.NewService(pg)
			alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		}
		if outbox == nil {
			outbox = notify.NewOutbox(pg)
		}
		if emailSender == nil {
			emailSender = notify.NewEmailSender(notify.EmailConfig{
				Host: cfg.SMTPHost, Port: cfg.SMTPPort,
				User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
			})
		}
		uptimeNotifier = &uptime.OutboxNotifier{
			Alerts:       alertSvc,
			Uptime:       uptimeSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// DepCounts — строка «Зависимых узлов: N» в down-уведомлении
			// корня (D3 Р9); тот же единственный Suppressor, что и у гейтов.
			DepCounts: depSuppressor,
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		}
		uptimeDetector = &uptime.Detector{
			Svc: uptimeSvc, Notifier: uptimeNotifier,
			Dep: depSuppressor, SettleGrace: settleGrace,
			// IncidentGroups — группы инцидентов (D3): хуки корня down-инцидента
			// монитора + членство uptime-детей через MarkSuppressedByDep.
			IncidentGroups: grouper,
			// Pool — лог шага 0 и адресность recovery (W3-E), см. Detector.Pool.
			Pool: pg,
		}
		// Ingestor нужен и режиму web (через него /probe/results проводит
		// результаты выносных проб), и режиму uptime (тот же хвост у
		// локальной пробы, Runner собирает его из своих полей) — детекция
		// инцидентов и запись в CH одинаковы для обоих источников.
		uptimeIngestor = &uptime.Ingestor{
			Svc:      uptimeSvc,
			Writer:   uptimeWriter,
			OnResult: uptimeDetector.OnResult,
		}
	}

	// Сборка кольца из cfg.SecretKey/SecretKeyPrev и раздача ВСЕМ трём
	// сервисам — единой точкой (wireSecretRing), а не по вызову
	// secretbox.NewKeyring + SetKeyring на каждый конструктор выше: та
	// раскладка была невидима для теста (проверялось только поведение
	// rewrapAllSecrets НАД уже собранным вручную в тесте кольцом, а не то,
	// что сам bootstrap строит и раздаёт кольцо правильно), и два регресса —
	// потерянный SecretKeyPrev при сборке кольца и пропущенная раздача
	// одному из сервисов — проходили мимо CI. Обязательно ДО подъёма
	// HTTP-слушателя и ДО бэкфилла ниже: итог по ротации обязан появиться в
	// логе того же рестарта, а не следующего (design §6/§7). Без ключа
	// (dev-дефолт) кольца нет и раздавать нечего — см. rationale (Info21) в
	// докстринге wireSecretRing.
	if cfg.SecretKey != devSecretKey {
		if _, err := wireSecretRing(cfg.SecretKey, cfg.SecretKeyPrev, orgSvc, alertSvc, uptimeSvc); err != nil {
			return err
		}
		rewrapAllSecrets(ctx, orgSvc, alertSvc, uptimeSvc)
	}

	// Планировщик — в любом процессе, у которого есть uptimeSvc, а не только
	// там, где крутится Runner.
	//
	// Постановка заданий раньше жила вторым тикером внутри Runner, а Runner
	// собирается только в uptime и all. В документированном раздельном
	// развёртывании web+ingest это значило, что очередь проверок не
	// наполнялась никогда: монитор показан включённым, состояние остаётся
	// unknown, пропуски heartbeat и истечение сертификатов не считаются, и ни
	// одной строки в логе. Выносные пробы там же простаивали — они честно
	// опрашивали пустую очередь.
	//
	// Постановка идемпотентна (ON CONFLICT DO NOTHING по (monitor_id, region))
	// и двигает last_scheduled_at только у мониторов, чьё задание реально
	// вставилось, поэтому несколько реплик с планировщиком расписание не
	// растягивают.
	if uptimeSvc != nil {
		uptimeScheduler := &uptime.Scheduler{Svc: uptimeSvc}
		// Живость Scheduler — та же логика, что у остальных фоновых циклов
		// (host.Evaluator и т.д.): молчание в журнале снаружи неотличимо от
		// «созревших проверок сейчас нет».
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_scheduler_last_tick_timestamp_seconds",
			"Unix time of the last completed uptime check scheduling pass. Stale value means due checks are not being enqueued.",
			nil, uptimeScheduler.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_scheduler_tick_duration_seconds",
			"Duration of the last uptime check scheduling call. Approaching scheduleBudget means PostgreSQL is not keeping up.",
			nil, uptimeScheduler.LastTickSeconds)
		go uptimeScheduler.Run(ctx)
		if cfg.Mode == "web" {
			// Симметрично предупреждению про оценщики: в этом процессе проверки
			// только ставятся в очередь, а исполнять их некому, кроме реплики
			// --mode=uptime или выносной пробы. Молчание тут читалось бы как
			// «аптайм работает».
			slog.Warn("uptime checks are scheduled here but NOT executed in this mode; "+
				"run a --mode=uptime (or --mode=all) replica, or register a remote probe",
				"mode", cfg.Mode)
		}
	}

	var runner *uptime.Runner
	if cfg.Mode == "uptime" || cfg.Mode == "all" {
		runner = &uptime.Runner{
			Svc:                 uptimeSvc,
			Writer:              uptimeWriter,
			Region:              cfg.LocalRegion,
			Concurrency:         cfg.UptimeConcurrency,
			OnResult:            uptimeDetector.OnResult,
			AllowPrivateTargets: cfg.SSRFAllowPrivateUptime,
		}
		// Живость Runner/Watchdog — та же логика, что у оценщиков ниже
		// (startEvaluators): молчание в журнале снаружи неотличимо от «проверок
		// к исполнению сейчас нет», умерший или отставший цикл иначе не виден.
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_runner_last_tick_timestamp_seconds",
			"Unix time of the last completed uptime check lease pass. Stale value means active checks are not being leased/executed.",
			nil, runner.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_runner_tick_duration_seconds",
			"Duration of the last uptime check lease call. Approaching leaseBudget means PostgreSQL is not keeping up.",
			nil, runner.LastTickSeconds)
		go runner.Run(ctx)

		watchdog := &uptime.Watchdog{
			Svc:      uptimeSvc,
			Detector: uptimeDetector,
			Notifier: uptimeNotifier,
			Writer:   uptimeWriter,
			Region:   cfg.LocalRegion,
			// Maint — живая проверка окна обслуживания перед напоминанием
			// (см. Watchdog.checkReminders); окна читает сам uptimeSvc.
			Maint: uptimeSvc,
		}
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_watchdog_last_tick_timestamp_seconds",
			"Unix time of the last completed heartbeat/reminder pass. Stale value means missed heartbeats and incident reminders are not being evaluated.",
			nil, watchdog.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_watchdog_tick_duration_seconds",
			"Duration of the last heartbeat/reminder pass. Approaching the interval means the watchdog stops keeping up.",
			nil, watchdog.LastTickSeconds)
		go watchdog.Run(ctx)

		// Оценщики регрессий производительности, правил по метрикам и регрессий
		// профилей — периодические джобы, которым нужны PG (инциденты/конфиг) и
		// общий outbox/каналы. К аптайму они отношения не имеют: они живут здесь
		// исторически, потому что тут уже собраны alertSvc, outbox и emailSender.
		//
		// Из-за этого при документированном раздельном развёртывании web+ingest
		// (оператор, которому аптайм не нужен, никогда не запустит --mode=uptime)
		// правило по метрике выглядело включённым и не вычислялось НИКОГДА —
		// ни строки в логе, ни признака в интерфейсе. Ровно тот же класс дефекта
		// уже находили у воркера доставки.
		//
		// GOTCHA_EVALUATORS_ENABLED позволяет включить их в любом режиме с БД. Дефолт
		// оставлен прежним намеренно: включать их автоматически везде значило бы
		// в связке web+uptime гонять двойную оценку.
		if runEvaluators(cfg) {
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender, &selfMetrics, depSuppressor, grouper, settleGrace, orgSvc)
		}

		slog.Info("uptime enabled", "region", cfg.LocalRegion, "concurrency", cfg.UptimeConcurrency)
	}

	// Оценщики вне режима uptime — только по явному включению.
	if cfg.Mode != "uptime" && cfg.Mode != "all" && cfg.Mode != "probe" {
		switch {
		case runEvaluatorsExplicit(cfg):
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender, &selfMetrics, depSuppressor, grouper, settleGrace, orgSvc)
			slog.Info("evaluators enabled by GOTCHA_EVALUATORS_ENABLED", "mode", cfg.Mode)
		default:
			// Молчать здесь нельзя: правило по метрике в интерфейсе выглядит
			// включённым, а не вычисляется. Оператор должен узнать об этом при
			// старте, а не во время инцидента. Хостовые пороги перечислены
			// наравне с остальными (ревью M3): startEvaluators поднимает и
			// host.Evaluator, а страница /hosts/settings точно так же
			// показывает пороги включёнными.
			//
			// GOTCHA_EVALUATORS_ENABLED гейтит ШЕСТЬ циклов, не четыре (W3-D,
			// запись 8 = находка W3-C): startEvaluators поднимает ещё
			// slo.Evaluator и escalation.Scheduler — их отсутствие в этой
			// строке молчало о том же классе дефекта, который сама строка
			// призвана объявлять. При раздельном развёртывании web+ingest без
			// GOTCHA_EVALUATORS_ENABLED SLO-алерты сжигания бюджета и эскалация
			// всех пяти прочих источников инцидентов молчат так же, как
			// правило по метрике, а прежняя строка об этом не предупреждала.
			slog.Warn(evaluatorsDisabledWarning, "mode", cfg.Mode)
		}
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	var spanWriter *trace.SpanWriter
	var metricWriter *metric.Writer
	var profileWriter *profile.Writer
	var logWriter *log.Writer
	// ingestHandler/webHandler объявлены здесь, а не через := в своих
	// if-блоках ниже: newRootMux собирается один раз, после того как оба
	// хендлера построены (или остались nil, если режим их не поднимает), а не
	// по кускам мимо каждого блока.
	var ingestHandler *ingest.Handler
	var webHandler *web.Handler
	// exportStore — очередь заявок на выгрузку (E1); объявлен здесь по тому же
	// поводу, что webHandler: конструируется ниже, в блоке `if outbox != nil`
	// (гейт по наличию ресурса — каталога выгрузок, а не по режиму процесса),
	// а используется дальше, в web-блоке, где строится webHandler.Exports.
	// nil остаётся, если каталог не удалось создать (см. ниже) — тогда раздел
	// выгрузок выключен, а не фатален для процесса.
	var exportStore *export.Store
	// exportWorkersWG считает горутины воркера и джанитора выгрузок (P2-OPS-5):
	// оба запускаются с ctx, отменяемым в SIGTERM/деплое, но раньше их
	// завершения никто не ждал — drain() ниже ждёт wg с ограниченным окном,
	// тем же приёмом, что и srv.Shutdown, чтобы заявка, прерванная посреди
	// сборки, успела дописать release() в БД до убийства процесса.
	var exportWorkersWG sync.WaitGroup
	// ingestSignalsWG — то же самое ожидание, что exportWorkersWG выше, но для
	// накопителя сигналов приёма (K7-5/K7-6): Run флашит по тику и делает
	// финальный Flush на отмену ctx, и это финальное Flush обязано успеть
	// дописать в PG до pg.Close() в drain() — иначе накопленное с последнего
	// тика теряется на каждой штатной остановке процесса.
	var ingestSignalsWG sync.WaitGroup
	// cardinality — ограничитель кардинальности; нужен и приёму (схлопывание),
	// и веб-слою (диагностика: что схлопнуто и примеры значений).
	var cardinality *ingest.CardinalityGuard
	// hostToucher — троттлер регистрации хостов (см. internal/host); объявлен
	// здесь (а не := внутри ingest-блока), чтобы веб-слой мог взять его для
	// Forget при удалении хоста, как ingestHandler/webHandler/cardinality выше.
	var hostToucher *host.Toucher
	// Доставка уведомлений и чистка очереди. Гейт — НЕ режим, а сам факт наличия
	// outbox: в очередь пишут контуры из разных режимов (uptime.OutboxNotifier в
	// web|uptime|all, оценщики трейсов/метрик/профилей в ingest|all), и когда
	// воркер жил только в ingest|all, инсталляция `--mode=uptime` открывала
	// инциденты, складывала задания в notification_outbox и НЕ ДОСТАВЛЯЛА ИХ
	// НИКОГДА — без единой строки в логе. Заодно не работал и janitor, поэтому
	// таблица с секретами каналов в payload росла безгранично.
	// notifyDirect — синхронная тест-отправка в канал из настроек (№69): те же
	// Senders/Secrets, что у воркера доставки. Остаётся nil на стендах без
	// outbox — кнопка теста там отвечает 404.
	var notifyDirect *notify.Direct
	if outbox != nil {
		senders := map[string]notify.Sender{
			alert.ChannelWebhook:  &notify.WebhookSender{AllowPrivate: cfg.SSRFAllowPrivateWebhook},
			alert.ChannelTelegram: &notify.TelegramSender{BaseURL: cfg.TelegramAPIBase, AllowPrivate: cfg.SSRFAllowPrivateTelegram},
		}
		if emailSender != nil && emailSender.Configured() {
			senders[alert.ChannelEmail] = emailSender
		} else {
			slog.Warn("GOTCHA_SMTP_HOST is not set, email alert channels are disabled")
		}
		notifyDirect = &notify.Direct{Senders: senders}
		if alertSvc != nil {
			notifyDirect.Secrets = alertSvc
		}
		// Наблюдение за доставкой. До него «алерт не пришёл» диагностировался
		// грепом логов, причём janitor через семь дней удалял улику.
		notifyStats := &notify.Stats{}
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_sent_total",
			"Notifications delivered successfully.", nil, notifyStats.Sent)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_failed_total",
			"Notifications given up on after exhausting retries.", nil, notifyStats.Failed)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_retried_total",
			"Delivery attempts that failed and were rescheduled.", nil, notifyStats.Retried)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_depth",
			"Notifications waiting to be delivered.", nil, notifyStats.Pending)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_failed",
			"Notifications in the queue that will not be retried again.", nil, notifyStats.FailedJobs)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_oldest_seconds",
			"Age of the oldest notification still waiting; the number that tells a quiet queue from a stuck one.",
			nil, notifyStats.OldestPendingAgeSeconds)
		go notifyStats.RunSnapshots(ctx, outbox)

		notifyWorker := &notify.Worker{
			Outbox:      outbox,
			Senders:     senders,
			Concurrency: cfg.NotifyConcurrency,
			Stats:       notifyStats,
		}
		// Секрет канала воркер достаёт по channel_id в момент отправки: в
		// payload задачи его больше нет (см. notify.SecretResolver). Проверка
		// на nil обязательна — присваивание nil-указателя *alert.Service в
		// интерфейс дало бы НЕ-nil интерфейс и панику на первой же доставке.
		if alertSvc != nil {
			notifyWorker.Secrets = alertSvc
		}
		go notifyWorker.Run(ctx)

		// Сводки о подавленных уведомлениях. Гейт тот же, что у воркера доставки
		// (наличие outbox, а не режим процесса): потолок без сводки — молчаливая
		// потеря, а «тишина в Telegram» неотличима от «всё спокойно».
		if alertSvc != nil {
			digester := &alert.Digester{
				Svc:          alertSvc,
				Outbox:       outbox,
				BaseURL:      cfg.BaseURL,
				EmailEnabled: emailSender != nil && emailSender.Configured(),
				Details:      detailPolicy(cfg),
				Locale:       i18n.Locale{Code: cfg.Locale},
			}
			go digester.Run(ctx)
		}

		// Чистка notification_outbox (техдолг): доставленные/проваленные строки
		// без ретенции копятся бесконечно.
		outboxJanitor := &notify.OutboxJanitor{
			Outbox:    outbox,
			Retention: time.Duration(cfg.OutboxRetentionDays) * 24 * time.Hour,
		}
		go outboxJanitor.Run(ctx)

		// Выгрузки ошибок/событий (E1). Гейт запуска — ДВА условия разом:
		// наличие ресурса (каталог на диске ЭТОГО процесса, включая пробу на
		// запись — exportDirWritable, P0-OPS-1) И режим, который отдаёт файл
		// (exportModeServesFiles: web|all — воркер и джанитор существуют
		// только чтобы обслуживать очередь webHandler'а, в --mode=ingest их
		// поднимать некому и незачем, см. докблок exportsWiringEnabled/
		// P1-OPS-2). exportsWiringEnabled формализует оба условия и покрыта
		// тестом. Каталог трогаем только когда режим вообще может им
		// воспользоваться: иначе на ingest/uptime/probe-реплике без тома
		// exportdata (в compose она read_only) это давало бы бесполезный warn
		// на каждый старт про раздел, которого в этом режиме нет вовсе.
		exportDirOK := false
		if exportModeServesFiles(cfg.Mode) {
			if err := ensureExportDir(cfg.ExportDir); err != nil {
				// Не фатально: продукт работает дальше без раздела выгрузок,
				// страница отвечает 404 (webHandler.Exports остаётся nil ниже).
				slog.Warn("export: export directory is unavailable, the exports section is disabled", "dir", cfg.ExportDir, "err", err)
			} else if err := exportDirWritable(cfg.ExportDir); err != nil {
				// MkdirAll молчит, если каталог уже существует, — в том числе
				// когда его создал Docker при монтировании тома и он достался
				// не тому владельцу (P0-OPS-1). Без этой пробы exportDirOK
				// стало бы true, а каждая заявка падала бы на OpenFile.
				slog.Warn("export: export directory is not writable by this process, the exports section is disabled", "dir", cfg.ExportDir, "err", err)
			} else {
				exportDirOK = true
			}
		}
		if exportsWiringEnabled(cfg.Mode, exportDirOK) {
			exportStore = export.NewStore(pg)
			exportCfg := export.Config{
				Dir:        cfg.ExportDir,
				TTL:        time.Duration(cfg.ExportTTLHours) * time.Hour,
				MaxRows:    cfg.ExportMaxRows,
				MaxBytes:   cfg.ExportMaxBytes,
				DiskBudget: cfg.ExportDiskBudgetBytes,
			}
			// В отличие от каталога (недоступность диска — условие среды, а не
			// ошибка оператора), кривая конфигурация — отказ старта: молчаливо
			// работающий воркер с MaxRows не строже защитного предела потока
			// событий теряет Truncated=true (см. Config.Validate).
			if err := exportCfg.Validate(); err != nil {
				return fmt.Errorf("export: configuration: %w", err)
			}
			// Как EmailEnabled у digester'а выше: nil вместо отправителя, если
			// почта не настроена — иначе EmailSender.Send диалит пустой
			// host:port на каждой терминальной заявке.
			var exportMailer export.Mailer
			if emailSender != nil && emailSender.Configured() {
				exportMailer = emailSender
			}
			exportWorker := &export.Worker{
				Store:  exportStore,
				Pool:   pg,
				Issues: export.NewIssueSource(issueSvc, cfg.BaseURL),
				Events: export.NewEventSource(event.NewQuery(ch), issueSvc),
				Cfg:    exportCfg,
				Notify: export.NewMailNotifier(exportMailer, exportStore, cfg.BaseURL, i18n.Locale{Code: cfg.Locale}),
			}
			exportWorkersWG.Add(1)
			go func() {
				defer exportWorkersWG.Done()
				exportWorker.Run(ctx)
			}()

			exportJanitor := &export.Janitor{
				Store:        exportStore,
				Pool:         pg,
				Dir:          cfg.ExportDir,
				RowRetention: exportRowRetention(exportCfg.TTL),
			}
			exportWorkersWG.Add(1)
			go func() {
				defer exportWorkersWG.Done()
				exportJanitor.Run(ctx)
			}()
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_janitor_last_tick_timestamp_seconds",
				"Unix time of the last completed export janitor pass. Stale value means expired exports, purge queue history, and orphan files are not being cleaned up.",
				nil, exportJanitor.LastTickUnix)
			selfMetrics.Add(selfmetrics.Gauge, "gotcha_export_janitor_tick_duration_seconds",
				"Duration of the last export janitor pass. Approaching the interval means PostgreSQL or the export directory is falling behind.",
				nil, exportJanitor.LastTickSeconds)

			// Наблюдаемость очереди выгрузок (P1-OPS-1): до этого КАЖДАЯ соседняя
			// очередь была видна (gotcha_notify_queue_depth/_queue_failed/
			// _queue_oldest_seconds выше, gotcha_purge_queue_depth/
			// _oldest_seconds ниже), а у выгрузок не было ни одной метрики —
			// дежурный не видел ни вставшую очередь, ни массовые отказы заявок
			// (сценарий закрытого P0 был именно таким: тишина, только slog.Warn
			// на тик воркера). Тот же приём, что у notifyStats.RunSnapshots(ctx,
			// outbox) выше: export.Stats симметричен notify.Stats.
			exportStats := &export.Stats{}
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_depth",
				"Export requests queued or being built.", nil, exportStats.Pending)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_failed",
				"Export requests in the queue that exhausted retries and will not be retried again.", nil, exportStats.FailedJobs)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_oldest_seconds",
				"Age of the oldest export request still queued or being built; the number that tells a quiet queue from a stuck one.",
				nil, exportStats.OldestPendingAgeSeconds)
			go exportStats.RunSnapshots(ctx, exportStore)

			// gotcha_storage_used_bytes был зарегистрирован только под
			// store="postgres" (pgUsedBytes выше), хотя registerUsedBytesMetric
			// принимает произвольный источник — каталог выгрузок остаётся
			// единственным куском диска, которым распоряжается само приложение,
			// и до этой строки единственным неизмеряемым.
			exportUsedBytes := registerUsedBytesMetric(&selfMetrics, "exports", exportDirUsedBytesSource{dir: cfg.ExportDir})
			go exportUsedBytes.Run(ctx)
		}
	}

	// Ретенция сущностей PostgreSQL. GOTCHA_EVENT_RETENTION_DAYS вытеснял только
	// телеметрию из ClickHouse: группы, инциденты и регрессии в PostgreSQL
	// жили вечно, и список проблем показывал группы, событий которых уже нет.
	//
	// Сроков четыре, и каждое правило живёт сроком СВОЕЙ сущности (см.
	// telemetry.entityRules): регрессия профиля — сроком профилей, инцидент
	// метрики — сроком метрик, инцидент аптайма — своим собственным. Раньше все
	// шесть правил жили одним GOTCHA_EVENT_RETENTION_DAYS.
	//
	// Гейт по наличию ресурса, а не по режиму: сущности переживают срок
	// хранения независимо от того, какие роли развёрнуты. Одновременный запуск
	// на нескольких репликах безопасен — проход берёт advisory-лок и на
	// занятом молча уступает.
	entityRetention := telemetry.Retentions{
		Events:      time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		Metrics:     time.Duration(cfg.MetricRetentionDays) * 24 * time.Hour,
		Profiles:    time.Duration(cfg.ProfileRetentionDays) * 24 * time.Hour,
		Incidents:   time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
		Deployments: time.Duration(cfg.DeployRetentionDays) * 24 * time.Hour,
	}
	if pg != nil && entityRetention.Any() {
		entityJanitor := &telemetry.EntityJanitor{
			Pool:      pg,
			Retention: entityRetention,
			// Хук на правило hosts: удаление хоста каскадит его host_incidents,
			// включая открытые, — то есть «сервер мёртв» исчезло бы без единого
			// события. Retirer перед удалением батча закрывает открытые инциденты
			// и рассылает уведомление о снятии с наблюдения (см. host.Retirer).
			// Нотифаер — тот же, что у оценщика хостов; alertSvc/outbox/
			// emailSender построены выше для всех режимов, доживающих сюда с БД
			// (ingest/web/all — блок orgSvc, uptime — блок uptimeSvc).
			PreDelete: map[string]telemetry.PreDeleteHook{
				"hosts": (&host.Retirer{
					Hosts:     host.NewStore(pg),
					Incidents: host.NewIncidentService(pg),
					Notifier: &host.HostNotifier{
						Alerts:       alertSvc,
						Outbox:       outbox,
						BaseURL:      cfg.BaseURL,
						EmailEnabled: emailSender.Configured(),
						Details:      detailPolicy(cfg),
						Locale:       i18n.Locale{Code: cfg.Locale},
						// Incidents/Hosts/Settings/Pool — эскалация (B4, T6):
						// StepNotifier перезагружает инцидент по ID (см.
						// HostNotifier.NotifyStep).
						Incidents: host.NewIncidentService(pg),
						Hosts:     host.NewStore(pg),
						Settings:  host.NewSettingsService(pg),
						Pool:      pg,
						// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
						Projects: escalation.OrgProjectNamer{Svc: orgSvc},
					},
				}).Retire,
			},
		}
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_entities_purged_total",
			"Rows deleted from PostgreSQL because they outlived the retention of the data they describe.",
			nil, entityJanitor.Purged)
		go entityJanitor.Run(ctx)
	}

	// Очистка телеметрии удалённых проектов. Заявку ставит та же транзакция,
	// что удаляет проект (org.DeleteProject/DeleteOrg); здесь — исполнитель,
	// который её разгребает, и суточная сверка сирот на случай, когда заявки не
	// появилось вообще. Гейт по обоим ресурсам: без PostgreSQL нет очереди, без
	// ClickHouse нечего удалять.
	//
	// Наблюдаемость обязательна, а не желательна: оператор, на котором лежит
	// обязанность удалить данные, должен видеть, что она не исполнена. Глубина
	// очереди и возраст самой старой заявки — две разные величины: одна заявка,
	// висящая третьи сутки, по глубине неотличима от только что поставленной.
	if pg != nil && ch != nil {
		purgeWorker := &telemetry.PurgeWorker{
			Queue:             telemetry.NewPurgeQueue(pg),
			Purger:            telemetry.NewPurger(ch),
			Conn:              ch,
			ReconcileInterval: time.Duration(cfg.PurgeReconcileHours) * time.Hour,
		}
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_purge_queue_depth",
			"Projects whose ClickHouse telemetry is still waiting to be deleted.",
			nil, purgeWorker.Depth)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_purge_queue_oldest_seconds",
			"Age of the oldest pending project purge request, in seconds.",
			nil, purgeWorker.OldestSeconds)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_projects_purged_total",
			"Projects whose ClickHouse telemetry has been deleted after the project was removed.",
			nil, purgeWorker.Purged)
		go purgeWorker.Run(ctx)
	}

	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		// maxBufBytes — per-writer байтовый потолок, один на всех писателей.
		// GOTCHA_MAX_WRITER_BUFFER_BYTES побеждает, если задан; иначе выводится из
		// обнаруженного потолка кучи (memLimitBytes), чтобы дефолтная поставка
		// без ручной настройки не могла сложить буферы в объём больше
		// heap-потолка — см. effectiveMaxBufferBytes. Сколько их, знает
		// autoBufferCapUnits, и это число под сторожем (internal/guards).
		maxBufBytes := effectiveMaxBufferBytes(cfg.MaxBufferBytes, memLimitBytes)
		if cfg.MaxBufferBytes == 0 && maxBufBytes > 0 {
			slog.Info("writer buffer cap auto-derived from detected heap ceiling",
				"bytes", maxBufBytes, "heap_ceiling_bytes", memLimitBytes)
		}

		batcher = event.NewBatcher(ch)
		batcher.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "events", batcher)
		go batcher.Run()

		// Трейсинг — часть ingest-контура: транзакции приезжают тем же
		// envelope-эндпойнтом, что и ошибки, и пишутся своим батчером
		// (transactions + spans), независимым от батчера событий.
		spanWriter = trace.NewSpanWriter(ch)
		spanWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "spans", spanWriter)
		go spanWriter.Run()

		// Метрики (этап 6) — третий приёмник ingest-контура: OTLP /v1/metrics
		// пишет точки в metric_points своим батчером.
		metricWriter = metric.NewWriter(ch)
		metricWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "metrics", metricWriter)
		// Точки со временем из будущего приводятся к моменту приёма (см.
		// metric.pointTime): для читателей они иначе равны потерянным. Счётчик
		// делает спешащие часы отправителя видимыми; имя хоста — в логе, не
		// в метке (кардинальность).
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_metric_points_clock_skew_total",
			"Metric points that arrived with a timestamp from the future and were clamped to the receive time. Growth means a sender's clock runs ahead of the server's.",
			nil, metric.ClockSkewPoints)
		go metricWriter.Run()

		// Профили (этап 7) — четвёртый приёмник: Sentry-профили из envelope и
		// pprof из /api/v1/profiles/pprof пишутся в profile_samples своим батчером.
		profileWriter = profile.NewWriter(ch)
		profileWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "profiles", profileWriter)
		go profileWriter.Run()

		// Логи (C1) — пятый приёмник: OTLP /v1/logs и NDJSON /api/v1/logs пишут в
		// logs своим батчером, тем же паттерном, что метрики и профили выше.
		logWriter = log.NewWriter(ch)
		logWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "logs", logWriter)
		go logWriter.Run()

		evaluator := &alert.Evaluator{
			Svc: alertSvc, Outbox: outbox, BaseURL: cfg.BaseURL, EmailEnabled: emailSender.Configured(),
			Details: detailPolicy(cfg),
			Locale:  i18n.Locale{Code: cfg.Locale},
			// Maint (B3) — окна обслуживания проекта: подавляет issue-алерты
			// (new_issue/regression/spike) ДО claimThrottle/claimBudget в
			// OnIssue, тем же приёмом, что pipeline.Maint ниже.
			Maint: uptime.NewService(pg),
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		}
		spikeWorker := &alert.Spike{
			Svc: alertSvc, Outbox: outbox, Issues: issueSvc, Events: event.NewQuery(ch), Evaluator: evaluator,
		}
		go spikeWorker.Run(ctx)

		// Детекторы производительности (план 3): находки уезжают в perf_issues
		// (PG) и алертят при первом обнаружении через тот же outbox, что и
		// алерты об ошибках. Пороги берутся из projects.perf_detector_config
		// через тот же кеш проектов, что читает transaction_sample_rate —
		// один инстанс на процесс, чтобы не держать два кеша одного и того же.
		projectCache := ingest.NewProjectCache(orgSvc)
		perfNotifier := &trace.OutboxNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			Pool:         pg, // perf_alert_throttle: рассылка ограничена по проекту
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
		}

		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.SetMaxQueueBytes(cfg.MaxQueueBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_depth",
			"Tasks waiting in the ingest pipeline queue.", nil, pipeline.Queued)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_bytes",
			"Bytes held by tasks waiting in the ingest pipeline queue.", nil, pipeline.QueuedBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_capacity",
			"Ingest pipeline queue capacity.", nil, pipeline.QueueCap)
		// По метрике на причину. Общий счётчик не различал переполнение
		// очереди (лечится размером очереди и числом воркеров) и отказ
		// хранилища (не лечится ничем из этого) — а видел оператор одно число.
		for _, reason := range ingest.DropReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_pipeline_dropped_tasks_total",
				"Tasks dropped by the ingest pipeline. These are lost for good; the reason label says why.",
				map[string]string{"reason": string(reason)},
				func() int64 { return pipeline.DroppedBy(reason) })
		}
		pipeline.Alerts = evaluator
		pipeline.Spans = spanWriter
		pipeline.Perf = trace.NewIssueService(pg)
		pipeline.PerfAlerts = perfNotifier
		// Maint (B3) — окна обслуживания проекта: подавляет только notify в
		// recordFinding, детекция/Record в perf_issues продолжает работать как
		// обычно. startEvaluators строит свой отдельный maint (не в scope
		// здесь, другая функция) тем же приёмом — uptime.NewService(pg)
		// требует только пул.
		pipeline.Maint = uptime.NewService(pg)
		pipeline.Projects = projectCache
		scrubber := ingest.NewScrubber(cfg.ScrubIP, cfg.ScrubEmail, cfg.ScrubKeys)
		scrubber.ScrubFreeText = cfg.ScrubFreeText // RA-L10: opt-in маскирование email в свободном тексте
		scrubber.SetAllowKeys(cfg.ScrubAllowKeys)  // явные исключения из fail-closed denylist
		pipeline.Scrub = scrubber
		// Wave 3 (sdd-audit-2026-08-12): дропы САМОГО пайплайна (переполнение
		// очереди, отказ PostgreSQL, паника обработчика — уже ПОСЛЕ того, как
		// grant списал квоту) раньше не попадали в org_usage.dropped_* вовсе —
		// их видел только process-local pipeline.Dropped(). ingestHandler.DropCounter
		// ниже покрывает квотные отказы (до постановки в очередь); тот же orgSvc
		// здесь закрывает вторую половину — Pipeline сам агрегирует per-org и
		// сливает пачкой по тику (см. Pipeline.DropCounter), а не пишет в БД на
		// каждый дроп.
		pipeline.DropCounter = orgSvc
		// Follow-up (2026-08-12): дропы БУФЕРА ПИСАТЕЛЯ (event.Batcher/
		// trace.SpanWriter при переполнении под перегрузкой) — тот же класс
		// потери, что дропы очереди выше, но другой слой. Писатель списывает
		// выброшенные строки их организациям через эти стоки, а Pipeline сливает
		// их тем же 60с-флашем в org_usage.dropped_* — единый путь до БД.
		batcher.SetDropSink(pipeline.CountDroppedEvents)
		spanWriter.SetDropSink(pipeline.CountDroppedTransactions)
		pipeline.Start()
		ingestHandler = ingest.NewHandler(
			ingest.NewKeyCache(orgSvc), ingest.NewOrgQuota(orgSvc), pipeline, cfg.MaxEventBytes)
		// №35: per-DSN лимит приёма из конфига; burst = 2×лимит — та же
		// пропорция, что у прежней захардкоженной пары 500/1000. 0 выключает.
		ingestHandler.SetRateLimit(time.Now, float64(cfg.IngestRateLimit), 2*float64(cfg.IngestRateLimit))
		// Квота транзакций — отдельный счётчик (organizations.transaction_quota
		// против org_usage.transactions_count): исчерпанный бюджет транзакций
		// не закрывает приём ошибок и наоборот.
		ingestHandler.TxQuota = ingest.NewOrgTransactionQuota(orgSvc)
		ingestHandler.Projects = projectCache
		// Метрики (этап 6): приёмник + отдельная квота метрик.
		ingestHandler.Metrics = metricWriter
		ingestHandler.MetricQuota = ingest.NewOrgMetricQuota(orgSvc)
		// Реестр хостов (A1): регистрирует host.name, приславшие метрики.
		// Троттлинг 1/мин на (project, host) — приём шлёт точки чаще, чем имеет
		// смысл писать в PG; потолок карты троттлера — защита от кардинального
		// мусора в самих именах хостов (см. host.Toucher).
		hostToucher = host.NewToucher(host.NewStore(pg), time.Minute, 65536)
		ingestHandler.Hosts = hostToucher
		// Регистрация хостов идёт в фоне, и её отказ на приёме не виден: пока
		// растут эти счётчики, last_seen не обновляется — прямой путь к
		// silent-инцидентам по живым машинам (failures) или к молчаливому
		// отсутствию новых машин в разделе (rejected).
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registration_failures_total",
			"Failed background upserts of the host registry. While this grows, host last_seen is stale and silence alerts may be false.",
			nil, hostToucher.UpsertFailures)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registrations_rejected_total",
			"New host names dropped because a project hit the per-project host ceiling.",
			nil, hostToucher.RejectedNames)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registrations_scope_skipped_total",
			"Metric exports carrying host.* attributes from a key type that may not register hosts. The export is accepted; only host registration is skipped.",
			nil, ingestHandler.HostScopeSkipped)
		// Профили (этап 7): приёмник + отдельная квота профилей.
		ingestHandler.Profiles = profileWriter
		ingestHandler.ProfileQuota = ingest.NewOrgProfileQuota(orgSvc)
		// Логи (C1): приёмник + отдельная квота логов.
		ingestHandler.Logs = logWriter
		ingestHandler.LogQuota = ingest.NewOrgLogQuota(orgSvc)
		// Деплои (C5): реестр выкладок из CI (PG-таблица deployments).
		ingestHandler.Deploy = deploy.NewStore(pg)
		ingestHandler.DropCounter = orgSvc
		// Отказы приёма по ключу (PROD-P?): раньше ни одна из веток
		// authenticate/otlpAuthenticate не была видна нигде — ни счётчиком, ни
		// логом. По метрике на причину — та же логика, что у
		// gotcha_pipeline_dropped_tasks_total выше: "ключа нет вовсе" (клиент
		// не настроил DSN) и "ключ чужого проекта" (скопированный не туда DSN)
		// требуют разных действий оператора/клиента, общий счётчик их не
		// различал бы. Причин семь (см. keyRejectReasons в internal/ingest).
		for _, reason := range ingest.KeyRejectReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_key_rejections_total",
				"Ingest requests rejected during key authentication, before quotas. The reason label says why.",
				map[string]string{"reason": string(reason)},
				func() int64 { return ingestHandler.KeyRejectedBy(reason) })
		}
		// J3: gotcha_ingest_key_rejections_total выше детализирует ТОЛЬКО отказ
		// по ключу (шесть узких причин), а частота/квота/размер тела не были
		// видны в метриках вовсе — только в логе конкретного эндпойнта, и без
		// возможности сравнить, какой ИЗ ШЕСТИ входов (событие, транзакция,
		// метрика, профиль, лог, деплой) отбивает больше. gotcha_ingest_rejected_total
		// даёт ту же картину огрублённо (одна причина key_unknown вместо
		// пяти), но КАЖДУЮ причину — со вторым измерением: видом телеметрии.
		// Обе метрики намеренно сосуществуют, разный масштаб детализации.
		for _, p := range ingest.IngestRejectionPairs() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_rejected_total",
				"Ingest requests rejected, by broad reason and telemetry signal. reason=\"key_revoked\" is reserved for future use (see ingest.IngestRejectReason) and never appears here today.",
				map[string]string{"reason": string(p.Reason), "signal": string(p.Signal)},
				func() int64 { return ingestHandler.RejectedBy(p.Reason, p.Signal) })
		}
		// E1: три собственных входа приёма переехали в /api/v1/*, старые пути
		// оставлены алиасами до 1.0 (см. ingest.DeprecatedPath). Счётчик отвечает
		// на единственный вопрос — какой из удаляемых путей ещё принимает трафик,
		// — чтобы проверка перед удалением алиасов была механической, а не
		// «вроде все переехали». Метка path, а не signal: вопрос про ПУТЬ, и
		// ответ на него не должен требовать похода в доку за соответствием
		// «сигнал → путь».
		//
		// Метрика ВРЕМЕННАЯ: она исчезает вместе с алиасами в 1.0. Это сказано
		// и в /docs/self-monitoring обеих локалей, и в CHANGELOG — вводить имя
		// self-метрики молча и удалять его через релиз значило бы сломать
		// контракт наблюдаемости, который мы сами же и заморозили.
		for _, p := range ingest.DeprecatedPaths() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_deprecated_path_total",
				"Requests that arrived on a deprecated ingest path, kept working as an alias until 1.0. TEMPORARY: this metric is removed together with the aliases.",
				map[string]string{"path": string(p)},
				func() int64 { return ingestHandler.DeprecatedPathHits(p) })
		}
		ingestHandler.Scrub = scrubber // RA-5: тем же скрабером чистим атрибуты метрик
		// Ограничитель кардинальности: один экземпляр на процесс, общий для всех
		// путей приёма — иначе один и тот же проект набирал бы отдельные наборы
		// значений на Sentry-входе и на OTLP-входе.
		cardinality = ingest.NewCardinalityGuard(
			cfg.CardinalityLimit, time.Duration(cfg.CardinalityWindowSeconds)*time.Second)
		ingestHandler.Cardinality = cardinality
		// Схлопнутое обязано быть видно оператору: без счётчика «часть имён
		// пропала» неотличимо от «их и не было».
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_cardinality_collapsed_total",
			"Field values collapsed into the overflow bucket because a project hit its cardinality limit.",
			nil, cardinality.CollapsedTotal)
		// Сколько памяти держит сама защита. Без этого числа её собственная
		// граница существовала только в виде произведения потолков — четырёх
		// миллиардов строк, то есть сотен гигабайт.
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_cardinality_tracked_values",
			"Distinct field values the cardinality guard is remembering right now.",
			nil, cardinality.TrackedValues)
		// Сигналы приёма per-project (аудит перед 1.0, K7-5/K7-6): отказ по
		// ключу и попадание на устаревший путь раньше были видны только
		// process-local self-метриками без метки проекта. Аккумулятор, а не
		// запись на каждый Touch — путь неаутентифицированный, запись в PG на
		// каждый отказ была бы усилителем нагрузки; Run сам флашит по тику и
		// делает финальный Flush при остановке процесса.
		ingestSignals := ingestsignal.NewRecorder(ingestsignal.NewStore(pg))
		ingestHandler.Signals = ingestSignals
		ingestSignalsWG.Add(1)
		go func() {
			defer ingestSignalsWG.Done()
			ingestSignals.Run(ctx)
		}()
		slog.Info("ingest enabled")
	}
	if cfg.Mode == "web" || cfg.Mode == "all" {
		authSvc := auth.NewService(pg)
		authSvc.Secure = strings.HasPrefix(cfg.BaseURL, "https://") // RA-L1: на HTTPS читать только __Host- cookie
		eventQuery := event.NewQuery(ch)
		webHandler = web.New(authSvc, orgSvc, issueSvc, eventQuery, cfg.BaseURL)
		webHandler.Alerts = alertSvc
		webHandler.Email = emailSender
		webHandler.EmailEnabled = emailSender.Configured()
		// Форма удаления ПДн должна знать, обезличиваются ли email/IP на приёме:
		// при включённом скрубинге поиск субъекта по ним не найдёт ничего.
		webHandler.ScrubIP = cfg.ScrubIP
		webHandler.ScrubEmail = cfg.ScrubEmail
		// exportStore остаётся nil, если каталог выгрузок недоступен (см. блок
		// export.Worker/Janitor выше) — тогда webHandler.Exports тоже nil, и
		// страница выгрузок отвечает 404, а не падает.
		webHandler.Exports = exportStore
		webHandler.ExportDir = cfg.ExportDir
		// Диагностика кардинальности: в режиме all это тот же экземпляр, что у
		// приёма. В раздельном развёртывании веб-узел его не видит — тогда
		// предупреждения доступны через /metrics и логи ingest-узла.
		webHandler.Cardinality = cardinality
		webHandler.Outbox = outbox
		webHandler.NotifyDirect = notifyDirect
		webHandler.NotifyLocale = i18n.Locale{Code: cfg.Locale}
		webHandler.Uptime = uptimeSvc
		webHandler.UptimeWriter = uptimeWriter
		// uptimeSvc is always built above whenever cfg.Mode is "web" or
		// "all" (see the uptimeSvc/uptimeWriter block), so UptimeQuery is
		// unconditional here too — same ClickHouse handle as uptimeWriter,
		// just for reads instead of writes.
		webHandler.UptimeQuery = uptime.NewQuery(ch)
		webHandler.UptimeIngestor = uptimeIngestor
		// Perf-страницы (этап 3, план 4): агрегаты транзакций из того же CH
		// (Trace) и связанные perf-проблемы из PG (PerfIssues).
		webHandler.Trace = trace.NewQuery(ch)
		webHandler.PerfIssues = trace.NewIssueService(pg)
		// Регрессии (этап 4, план 5): список /projects/{id}/regressions читает
		// perf_regressions из PG (тот же сервис, что и оценщик выше).
		webHandler.Regressions = trace.NewRegressionService(pg)
		webHandler.Metrics = metric.NewQuery(ch)
		webHandler.MetricRules = metric.NewRuleService(pg)
		webHandler.MetricIncidents = metric.NewIncidentService(pg)
		// Хосты (план A1, задача 14): реестр + инциденты + пороги — всегда
		// вместе с Metrics (страницы хостов читают и то, и другое).
		webHandler.Hosts = host.NewStore(pg)
		webHandler.HostIncidents = host.NewIncidentService(pg)
		webHandler.HostSettings = host.NewSettingsService(pg)
		// HostOverrides/GroupThresholds (план B2): та же ниша, что тройка выше
		// — карточка хоста показывает и правит per-host override поверх
		// каскада host→role→env→project→default (ThresholdResolver).
		webHandler.HostOverrides = host.NewHostOverrideService(pg)
		webHandler.GroupThresholds = host.NewGroupThresholdService(pg)
		// hostToucher остаётся nil в чистом web-режиме (создаётся только в
		// ingest-блоке выше) — тогда HostForget остаётся настоящим nil-
		// интерфейсом (см. комментарий поля), а не typed-nil, на котором
		// вызов Forget запаниковал бы.
		if hostToucher != nil {
			webHandler.HostForget = hostToucher
		}
		// Логи (C2, задача 2): просмотрщик /projects/{id}/logs читает из того же
		// CH-подключения, что и остальные телеметрийные разделы; LogRetentionDays —
		// реальный TTL хранения (обрезает окно снизу, см. web.parseLogFilter),
		// не только справочный дефолт.
		webHandler.LogQuery = log.NewQuery(ch)
		webHandler.LogRetentionDays = cfg.LogRetentionDays
		// Деплои (C5): store читается веб-слоем для маркеров на графиках,
		// экрана-списка и привязки регрессий (тот же пул, что у ingest.Deploy).
		webHandler.Deploy = deploy.NewStore(pg)
		// Сигналы приёма per-project (аудит перед 1.0, K7-5/K7-6): та же
		// таблица, что пишет Recorder в ingest-блоке выше — Store сам не
		// хранит состояния, отдельный экземпляр на веб-узел ничего не делит с
		// Recorder, кроме пула. В раздельном развёртывании web/ingest веб-узел
		// читает те же строки, что писал ingest-узел через общую PG.
		webHandler.Signals = ingestsignal.NewStore(pg)
		// SLO (план D1): раздел /projects/{id}/slos читает определения из PG
		// (SLO) и считает достижение/остаток бюджета за окно теми же
		// провайдерами, что и оценщик (availability/latency из trace,
		// uptime из uptime.Query; окна обслуживания не жгут бюджет).
		webHandler.SLO = slo.NewStore(pg)
		webHandler.SLOProviders = slo.Providers(trace.NewQuery(ch), uptime.NewQuery(ch), uptime.NewService(pg), cfg.RetentionDays)
		// Эскалации (B4, задача 9): /projects/{id}/escalations читает и правит
		// ту же политику, что резолвят оценщики (startEvaluators заводит свой
		// отдельный NewPolicyStore(pg) — это дешёвый объект без состояния
		// поверх пула, плодить общий синглтон между стартом оценщиков и
		// web-обвязкой не требуется).
		webHandler.EscalationPolicy = escalation.NewPolicyStore(pg)
		// Подавление шторма (B5, задача 9): /projects/{id}/alert-suppression
		// читает/правит рёбра зависимостей тем же Store, что использует
		// depSuppressor выше (независимый объект без состояния поверх пула,
		// тот же принцип, что и у EscalationPolicy).
		webHandler.AlertDeps = depsuppress.NewStore(pg)
		// Лента инцидентов (D3, задача 9): /projects/{id}/incident-feed читает
		// те же группы, что пишет Grouper и подчищает janitor, — тем же
		// groupStore выше, а не собственным экземпляром: один Store на
		// подсистему, как у Uptime/SLO.
		webHandler.IncidentGroups = groupStore
		// SuppressionGrace — та же задержка первого уведомления
		// (GOTCHA_DEPENDENCY_SETTLE_SECONDS), что задаёт settleGrace для
		// depSuppressor/uptime.Detector/escalation.Scheduler выше: экран
		// подавления шторма показывает оператору фактически действующую
		// величину, а не догадку (P2-1 устранения аудита B5).
		webHandler.SuppressionGrace = settleGrace
		webHandler.Profiles = profile.NewQuery(ch)
		webHandler.ProfileRegressions = profile.NewRegressionService(pg)
		webHandler.OAuth = buildRegistry(cfg)
		// Ключ подписи oauth-flow cookie выводим отдельным подключом от мастера,
		// а не берём мастер напрямую: тогда контекст HMAC-подписи cookie и
		// контекст at-rest-шифрования SSO client_secret (sha256(master) в org)
		// не делят один и тот же ключевой материал — доменное разделение (Info20).
		// Ephemeral oauth-cookie при апгрейде просто инвалидируются один раз.
		webHandler.SecretKey = deriveCookieKey(cfg.SecretKey)
		webHandler.TrustedProxies = cfg.TrustedProxies
		webHandler.RegistrationMode = cfg.RegistrationMode
		webHandler.HSTSHeader = web.HSTSHeaderValue(
			cfg.HSTSEnabled, cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains, cfg.HSTSPreload)
		webHandler.RetentionDays = cfg.RetentionDays
		webHandler.SpanRetentionDays = cfg.SpanRetentionDays
		webHandler.LocalRegion = cfg.LocalRegion
		webHandler.Purger = telemetry.NewPurger(ch)
		// Раздача install.sh/бинарей агента (план A2, задача 10): каталог из
		// GOTCHA_DIST_DIR, дефолт совпадает с путём из Dockerfile —
		// см. AgentDistDir. Порог её лимитера (ops-H4) — отдельно от New(),
		// чтобы Ansible-раскатка/массовое обновление парка за одним IP не
		// упирались в дефолт, рассчитанный на штучные установки.
		webHandler.AgentDistDir = cfg.AgentDistDir
		webHandler.SetAgentDistRateLimit(cfg.AgentDistRatePerMin)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_web_cross_origin_rejected_total",
			"POST requests rejected because Origin/Referer did not match GOTCHA_BASE_URL.",
			nil, webHandler.CrossOriginRejected)
		// C3: /uptime/hb/{token} — публичная ссылка, разосланная в cron'ы, а
		// такие ссылки регулярно дёргают не люди (unfurl-бот мессенджера,
		// антивирусный прокси, префетч браузера). Не в счёт факта живости
		// монитора — но и не невидимо: причина отсева на self-метрике.
		for _, reason := range web.HeartbeatIgnoreReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_uptime_heartbeat_ignored_total",
				"Heartbeat pings received but NOT counted as monitor liveness. The reason label says why (prefetch/preview header or a known link-preview bot User-Agent).",
				map[string]string{"reason": string(reason)},
				func() int64 { return web.HeartbeatIgnoredBy(reason) })
		}
		janitor := &auth.Janitor{Svc: authSvc}
		if orgSvc != nil {
			// Просроченные/принятые инвайты копят email приглашённых бессрочно —
			// чистим на том же тике (минимизация ПДн, 152-ФЗ ст.5 ч.7).
			janitor.Extra = append(janitor.Extra, auth.Cleanup{
				Name: "expired invites", Fn: orgSvc.PurgeExpiredInvites,
			})
		}
		go janitor.Run(ctx)
		slog.Info("web enabled")
	}

	// Корневой mux и сервер — вынесены в server.go (newRootMux/newServer):
	// сборка проводки сама по себе не содержит логики run(), а тест заголовков
	// и тест проб должны собирать ровно то, что слушает порт в проде, а не
	// свою копию.
	mux := newRootMux(rootDeps{
		pg:            pg,
		ch:            ch,
		selfMetrics:   &selfMetrics,
		ingestHandler: ingestHandler,
		webHandler:    webHandler,
	})
	srv := newServer(&cfg, mux)
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "mode", cfg.Mode)
		errCh <- srv.ListenAndServe()
	}()

	// drain останавливает все фоновые писатели/воркеры при завершении.
	// Независимые цепочки ждутся ПАРАЛЛЕЛЬНО (I3, финревью волны 1 аудита
	// перед 1.0): раньше все бюджеты складывались последовательно — сумма
	// ограниченных ожиданий (без runner.Close(), который был вовсе без
	// потолка) доросла до 90с, ровно stop_grace_period в docker-compose.yml
	// — запаса не оставалось, и превышение любого бюджета гарантированно
	// добивало остановку до SIGKILL. Верхняя граница сейчас (после
	// srv.Shutdown, 10с, которое отрабатывает ДО вызова drain — см. select
	// ниже) — max по параллельным веткам. Каждый Close(ctx) писателя сперва
	// ждёт <-w.done (in-flight flushWithTimeout, до 10с своим бюджетом —
	// event/batcher.go, trace/writer.go, metric/writer.go, log/writer.go,
	// profile/writer.go, uptime/results.go), и лишь потом входит в свой
	// ctx-ограниченный цикл долива — до 20с на писателя. Самая длинная
	// цепочка pipeline→batcher→spanWriter: 10+20+20 = 50с. Итог 10+50 = 60с,
	// с запасом 30с до 90с грейса.
	//
	// Параллелить можно только НЕЗАВИСИМЫЕ писатели — порядок внутри двух
	// цепочек ниже сохранён, потому что он не декоративный:
	//   - pipeline forward'ит задания в batcher (events) и spanWriter
	//     (Pipeline.Spans = spanWriter, ingest.NewPipeline(_, batcher)):
	//     закрыть их раньше pipeline.Close() значило бы, что воркер
	//     пайплайна мог бы писать в уже закрытый писатель;
	//   - runner пишет результаты проверок в uptimeWriter
	//     (Runner.Writer, Ingestor.Accept → Writer.Add): та же причина.
	// metricWriter/profileWriter/logWriter пишутся НАПРЯМУЮ из
	// ingest-хендлера (ingestHandler.Metrics/Profiles/Logs), в обход
	// pipeline, — их закрытие ни от чего из перечисленного не зависит.
	drain := func() {
		var branches []func()

		if pipeline != nil || batcher != nil || spanWriter != nil {
			branches = append(branches, func() {
				if pipeline != nil {
					// Тот же бюджет, что у остальных писателей: без дедлайна
					// деградация PostgreSQL держала бы shutdown до ~20 минут.
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := pipeline.Close(cctx); err != nil {
						slog.Warn("ingest pipeline drain incomplete", "error", err)
					}
					cancel()
				}
				if batcher != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := batcher.Close(cctx); err != nil {
						slog.Error("event batcher drain failed", "error", err)
					}
				}
				if spanWriter != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := spanWriter.Close(cctx); err != nil {
						slog.Error("span writer drain failed", "error", err)
					}
				}
			})
		}
		if metricWriter != nil {
			branches = append(branches, func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := metricWriter.Close(cctx); err != nil {
					slog.Error("metric writer drain failed", "error", err)
				}
			})
		}
		if profileWriter != nil {
			branches = append(branches, func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := profileWriter.Close(cctx); err != nil {
					slog.Error("profile writer drain failed", "error", err)
				}
			})
		}
		if logWriter != nil {
			branches = append(branches, func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := logWriter.Close(cctx); err != nil {
					slog.Error("log writer drain failed", "error", err)
				}
			})
		}
		// Аккумулятор сигналов приёма (K7-5/K7-6): ctx уже отменён (см. select
		// ниже), Run() уже увидел <-ctx.Done() и делает финальный Flush — см.
		// drainIngestSignals.
		branches = append(branches, func() {
			drainIngestSignals(&ingestSignalsWG)
		})
		if runner != nil || uptimeWriter != nil {
			branches = append(branches, func() {
				if runner != nil {
					// runner.Close() у самого Runner без ctx (докблок
					// «идемпотентен» — внутренний контракт здесь не
					// меняется), ограничиваем на месте вызова.
					if !closeBounded(runner.Close, 10*time.Second) {
						slog.Warn("uptime runner did not stop within the shutdown window")
					}
				}
				if uptimeWriter != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := uptimeWriter.Close(cctx); err != nil {
						slog.Error("uptime result writer drain failed", "error", err)
					}
				}
			})
		}
		// Воркер/джанитор выгрузок (P2-OPS-5): ctx уже отменён к этому моменту
		// (см. select ниже), сами они завершаются быстро — тик воркера видит
		// <-ctx.Done() и выходит из select в Run() без ожидания текущего
		// тика/файла. Ждём именно ЗАВЕРШЕНИЯ горутин, а не только отмены ctx:
		// без этого release() (см. internal/export/worker.go) мог бы не успеть
		// дописать строку в PG до того, как процесс убьют. Окно небольшое
		// (5с, не 15 минут jobTimeout) — ждать саму сборку файла нельзя, это
		// сорвало бы деплой; 5с с запасом хватает release()/detachTimeout()
		// (см. их докблоки).
		branches = append(branches, func() {
			if !waitGroupWithTimeout(&exportWorkersWG, 5*time.Second) {
				slog.Warn("export worker/janitor did not stop within the shutdown window")
			}
		})

		drainParallel(branches...)
	}

	select {
	case err := <-errCh:
		drain()
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			drain()
			return err
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			drain()
			return err
		}
		drain()
		return nil
	}
}

// wireSecretRing — единая точка «проводки» кольца at-rest ключей: и сборка
// (secretbox.NewKeyring из current/previous), и раздача всем трём сервисам,
// которые шифруют секреты (SSO client_secret, alert-каналы, HTTP-заголовки
// мониторов). Вынесена отдельной функцией из run() тем же приёмом, что и
// rewrapAllSecrets ниже: обе половины проводки, размазанные инлайном по
// run() (сборка кольца — до всех сервисов, раздача — по четырём местам их
// конструирования, разным для разных --mode), были невидимы для теста —
// TestRewrapAllSecretsRotationRoundTrip проверял только поведение
// rewrapAllSecrets НАД уже вручную собранным в тесте кольцом, а не то, что
// run() САМ строит и раздаёт кольцо правильно. Два регресса проходили с
// зелёными build/vet/test молча: (1) секрет-ключ ротации собирался как
// NewKeyring(cfg.SecretKey, "") — кольцо теряет предыдущий ключ, ротация
// перестаёт работать без единого сигнала об этом; (2) один из четырёх
// SetKeyring пропадал — соответствующий сервис не видит старый ключ на
// время ротации. С публично известным dev-дефолтом шифровать бессмысленно —
// ключ виден в исходниках, а «enc:»-значение давало бы ложное чувство
// защиты (Info21), поэтому вызывающий (run()) зовёт эту функцию только
// когда cfg.SecretKey != devSecretKey; здесь это не перепроверяется.
//
// nil-сервис пропускается молча — тем же контрактом, что у rewrapAllSecrets:
// в части режимов процесса соответствующий сервис не строится вовсе.
func wireSecretRing(current, previous string, orgSvc *org.Service, alertSvc *alert.Service, uptimeSvc *uptime.Service) (secretbox.Keyring, error) {
	ring, err := secretbox.NewKeyring(current, previous)
	if err != nil {
		return secretbox.Keyring{}, fmt.Errorf("build secretbox keyring: %w", err)
	}
	// id ключей — не секрет, но без старого оператору неоткуда взять
	// <old-id> для проверочного SELECT из процедуры ротации (privacy.md):
	// previous_key_id логируется только пока ротация идёт (PreviousID
	// пуст без предыдущего ключа), current_key_id — всегда.
	slog.Info("secretbox keyring ready", "current_key_id", ring.CurrentID(),
		"previous_key_id", ring.PreviousID(),
		"rotation_in_progress", previous != "")
	if orgSvc != nil {
		orgSvc.SetKeyring(ring)
	}
	if alertSvc != nil {
		alertSvc.SetKeyring(ring)
	}
	if uptimeSvc != nil {
		uptimeSvc.SetKeyring(ring)
	}
	return ring, nil
}

// rewrapAllSecrets — бэкфилл секретов трёх хранилищ на текущий ключ кольца
// (design §6): org_sso.client_secret, alert_channels.secret, заголовки
// HTTP-мониторов. Вынесена отдельной функцией из run(), а не оставлена
// инлайн-циклом — так порядок «после раздачи кольца, до подъёма слушателя» и
// свойство «ошибка прохода не роняет старт» можно проверить тестом отдельно
// от всего бутстрапа. Сигнатура без возврата ошибки — намеренно: решение «не
// ронять старт при отказе SQL» принимает вызывающий (run()), но раз функция
// физически не может вернуть ошибку наверх, эту границу нельзя случайно
// нарушить последующей правкой.
//
// nil-сервис пропускается молча — в части режимов процесса (например,
// --mode=probe) соответствующий сервис не строится вовсе.
func rewrapAllSecrets(ctx context.Context, orgSvc *org.Service, alertSvc *alert.Service, uptimeSvc *uptime.Service) {
	if orgSvc != nil {
		if n, err := orgSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap org sso secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped org sso secrets", "rows", n)
		}
	}
	if alertSvc != nil {
		if n, err := alertSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap alert channel secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped alert channel secrets", "rows", n)
		}
	}
	if uptimeSvc != nil {
		if n, err := uptimeSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap monitor header secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped monitor header secrets", "rows", n)
		}
	}
}

// startEvaluators запускает периодические оценщики: регрессии
// производительности, правила по метрикам и регрессии профилей. Вынесено в
// функцию, потому что запускать их надо из двух мест — режима uptime|all и
// явного GOTCHA_EVALUATORS_ENABLED в прочих режимах с БД.
func startEvaluators(ctx context.Context, cfg Config, pg *pgxpool.Pool, ch driver.Conn,
	alertSvc *alert.Service, outbox *notify.Outbox, emailSender *notify.EmailSender,
	selfMetrics *selfmetrics.Registry, dep *depsuppress.Suppressor, grouper *incidentgroup.Grouper,
	settleGrace time.Duration, orgSvc *org.Service) {
	// maint — окна обслуживания проекта (план B3): подавляет open/close-
	// уведомления инцидентов всех источников оценщиков ниже (host сейчас,
	// metric/trace/profile/slo следом). uptimeSvc:385 здесь НЕ переиспользуем —
	// он строится только в режимах uptime|all, а startEvaluators зовётся и без
	// них (GOTCHA_EVALUATORS_ENABLED); uptime.NewService(pg) требует только пул,
	// тот же приём уже применён ниже для sloEval (:1216).
	maint := uptime.NewService(pg)

	// policyStore — политика эскалации (B4, T7): одна на процесс, передаётся во
	// все 5 оценщиков ниже — каждый резолвит свою лесенку (project, severity)
	// на открытии инцидента (см. Evaluator.notifyOpen каждого пакета).
	policyStore := escalation.NewPolicyStore(pg)

	evaluator := &trace.Evaluator{
		Pool:        pg,
		Query:       trace.NewQuery(ch),
		Regressions: trace.NewRegressionService(pg),
		Maint:       maint,
		Policy:      policyStore,
		Notifier: &trace.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// Regressions/Pool — эскалация (B4, T6): StepNotifier перезагружает
			// регрессию по ID (см. RegressionNotifier.NotifyStep).
			Regressions: trace.NewRegressionService(pg),
			Pool:        pg,
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		},
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_trace_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed performance regression evaluation pass. Stale value means regression alerts are not being evaluated.",
		nil, evaluator.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_trace_evaluator_tick_duration_seconds",
		"Duration of the last performance regression evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, evaluator.LastTickSeconds)
	go evaluator.Run(ctx)

	// Оценщик пороговых алертов на метрики (этап 6, план 4) — та же ниша, что
	// и regression-evaluator: периодическая джоба на PG (правила/инциденты) +
	// CH (агрегаты метрик), алертит через общий outbox. alertSvc/outbox/
	// emailSender построены выше в этом же блоке (uptime|all).
	metricEval := &metric.Evaluator{
		Rules:     metric.NewRuleService(pg),
		Query:     metric.NewQuery(ch),
		Incidents: metric.NewIncidentService(pg),
		Maint:     maint,
		Policy:    policyStore,
		Pool:      pg,
		// IncidentGroups — группы инцидентов (D3): членство metric-инцидента
		// правила label_key='host' в группе down-корня его хоста.
		IncidentGroups: grouper,
		Notifier: &metric.MetricNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// Incidents/Rules/Pool — эскалация (B4, T6): StepNotifier
			// перезагружает инцидент+правило по ID (см. MetricNotifier.NotifyStep).
			Incidents: metric.NewIncidentService(pg),
			Rules:     metric.NewRuleService(pg),
			Pool:      pg,
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.MetricEvalInterval) * time.Second,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_metric_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed metric threshold evaluation pass. Stale value means metric rule alerts are not being evaluated.",
		nil, metricEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_metric_evaluator_tick_duration_seconds",
		"Duration of the last metric threshold evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, metricEval.LastTickSeconds)
	go metricEval.Run(ctx)

	// Оценщик регрессий профилей (этап 9): рост self-CPU доли функции над
	// скользящей базой → инцидент + алерт через общий outbox. Та же ниша,
	// что regression/metric-оценщики; alertSvc/outbox/emailSender/pg/ch в scope.
	profileRegEval := &profile.RegressionEvaluator{
		Query:       profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pg),
		Maint:       maint,
		Policy:      policyStore,
		Pool:        pg,
		Notifier: &profile.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// Regressions/Pool — эскалация (B4, T6): StepNotifier перезагружает
			// регрессию по ID (см. RegressionNotifier.NotifyStep).
			Regressions: profile.NewRegressionService(pg),
			Pool:        pg,
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.ProfileEvalInterval) * time.Second,
		Config:   profile.DefaultProfileRegressionConfig(),
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_profile_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed profile regression evaluation pass. Stale value means profile regression alerts are not being evaluated.",
		nil, profileRegEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_profile_evaluator_tick_duration_seconds",
		"Duration of the last profile regression evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, profileRegEval.LastTickSeconds)
	go profileRegEval.Run(ctx)

	// Оценщик встроенных порогов хоста (диск/память/нагрузка/тишина, план A1) —
	// та же ниша, что metric/profile-оценщики выше, но правила не в БД, а
	// фиксированный набор из четырёх видов + настройки проекта.
	hostEval := &host.Evaluator{
		Store:     host.NewStore(pg),
		Settings:  host.NewSettingsService(pg),
		Incidents: host.NewIncidentService(pg),
		Metrics:   metric.NewQuery(ch),
		Overrides: host.NewHostOverrideService(pg),
		Groups:    host.NewGroupThresholdService(pg),
		Maint:     maint,
		Policy:    policyStore,
		Pool:      pg,
		// Dep — гейт зависимостей (B5, T8): подавляет инцидент хоста, пока у
		// его задекларированного родителя открыт свой.
		Dep: dep,
		// IncidentGroups — группы инцидентов (D3): членство host-инцидентов +
		// хуки silent-корня (открытие/ретро/закрытие группы).
		IncidentGroups: grouper,
		Notifier: &host.HostNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// Incidents/Hosts/Settings/Pool — эскалация (B4, T6): StepNotifier
			// перезагружает инцидент/хост/настройки по ID (см.
			// HostNotifier.NotifyStep).
			Incidents: host.NewIncidentService(pg),
			Hosts:     host.NewStore(pg),
			Settings:  host.NewSettingsService(pg),
			Pool:      pg,
			// DepCounts — строка «Зависимых узлов: N» в open-уведомлении
			// silent-корня (D3 Р9); тот же единственный Suppressor, что и у
			// Dep выше. У Retirer-экземпляра HostNotifier (entityJanitor)
			// поле остаётся nil намеренно — он шлёт только retire/close.
			DepCounts: dep,
			// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.HostEvalInterval) * time.Second,
	}
	// Живость оценщика хостов. Раньше о нём было известно ровно одно: в журнале
	// нет ошибок. Умерший или отстающий оценщик выглядит снаружи в точности как
	// «на хостах всё спокойно» — тишина и есть его нормальный вывод, поэтому
	// наблюдать надо не отказ, а сам факт продолжения работы.
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_host_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed host threshold evaluation pass. Stale value means host alerts are not being evaluated.",
		nil, hostEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_host_evaluator_tick_duration_seconds",
		"Duration of the last host threshold evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, hostEval.LastTickSeconds)
	go hostEval.Run(ctx)

	// Оценщик error budget / burn rate по SLO (план D1): та же ниша, что
	// metric/host/profile-оценщики — периодическая джоба поверх PG (определения+
	// инциденты) и CH (ряды good/total через провайдеры). uptime.Service/Query
	// здесь не построены (в отличие от aптайм-воркера), поэтому конструируем их
	// под провайдеры: Service — окна обслуживания (не жгут бюджет), Query —
	// ряд успешных проверок для uptime-SLO. Нотифаер шлёт алерт сжигания бюджета
	// по тем же каналам проекта, что и остальные (regression/uptime/host).
	sloNotifier := &slo.SLOBurnNotifier{
		Alerts:       alertSvc,
		Outbox:       outbox,
		BaseURL:      cfg.BaseURL,
		EmailEnabled: emailSender.Configured(),
		Details:      detailPolicy(cfg),
		Locale:       i18n.Locale{Code: cfg.Locale},
		// Store/Pool — эскалация (B4, T6): StepNotifier перезагружает
		// SLO+инцидент по ID (см. SLOBurnNotifier.NotifyStep).
		Store: slo.NewStore(pg),
		Pool:  pg,
		// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
		Projects: escalation.OrgProjectNamer{Svc: orgSvc},
	}
	sloEval := &slo.Evaluator{
		Pool:      pg,
		Store:     slo.NewStore(pg),
		Providers: slo.Providers(trace.NewQuery(ch), uptime.NewQuery(ch), uptime.NewService(pg), cfg.RetentionDays),
		Notifier:  sloNotifier,
		Interval:  time.Duration(cfg.SLOEvalInterval) * time.Second,
		Maint:     maint,
		Policy:    policyStore,
		// IncidentGroups — группы инцидентов (D3): членство uptime-SLO-инцидента
		// в группе down-корня его монитора.
		IncidentGroups: grouper,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_slo_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed SLO burn-rate evaluation pass. Stale value means SLO error-budget alerts are not being evaluated.",
		nil, sloEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_slo_evaluator_tick_duration_seconds",
		"Duration of the last SLO burn-rate evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, sloEval.LastTickSeconds)
	go sloEval.Run(ctx)

	// Централизованный планировщик эскалаций (B4, T8): один тикер вместо
	// того, чтобы каждый из пяти оценщиков выше сам гонял свою лесенку —
	// эскалация ортогональна открытию инцидента (оценщик открывает его один
	// раз, а лесенка идёт своим шагом, пока инцидент открыт и не
	// подтверждён). Src/Notifier каждого биндинга — те же объекты, что
	// собраны выше для соответствующего оценщика (Regressions/Incidents/
	// Store как Source (T4), Notifier как StepNotifier (T6)).
	// uptime — W2-C находка 2 (аудит 2026-08-27): раньше единственный из 6
	// источников инцидентов вне контура эскалации. Src=maint (uptime.Service,
	// уже построен выше для окон обслуживания) — тот же приём, что и sloEval
	// делает для своих провайдеров: startEvaluators не переиспользует
	// uptimeSvc/uptimeNotifier из run() (другая функция, другая область
	// видимости, см. её комментарий про uptimeSvc:385), строит свой
	// экземпляр. Notifier — uptime.OutboxNotifier.NotifyStep: ОДНАКО первую
	// доставку "down" (уровень 0) ступени по-прежнему шлёт Detector
	// (uptime.Detector.notify, свой B5-автомат held/grace/ретрай — W2-C
	// находка 1), а не эта лесенка — uptime.Service.OpenUnacked нарочно не
	// отдаёт планировщику инциденты на escalation_level=0, чтобы два гейта
	// (Detector.settleHeldIncident и Scheduler.tickOne) не решали одну и ту
	// же задачу над одной и той же строкой на разных тикерах и не отправили
	// "down" дважды — см. докблок uptime.Service.OpenUnacked. С уровня 1 эта
	// лесенка эскалирует дальше как обычно.
	uptimeEscNotifier := &uptime.OutboxNotifier{
		Alerts:       alertSvc,
		Uptime:       maint,
		Outbox:       outbox,
		BaseURL:      cfg.BaseURL,
		EmailEnabled: emailSender.Configured(),
		Details:      detailPolicy(cfg),
		Locale:       i18n.Locale{Code: cfg.Locale},
		// DepCounts — строка «Зависимых узлов: N» в эскалированном
		// уведомлении (D3 Р9), тот же единственный Suppressor, что и Dep
		// ниже.
		DepCounts: dep,
		// Projects — имя проекта в теме/теле/webhook-payload (W3-E).
		Projects: escalation.OrgProjectNamer{Svc: orgSvc},
	}
	bindings := []escalation.Binding{
		{Src: trace.NewRegressionService(pg), Notifier: evaluator.Notifier},
		{Src: metric.NewIncidentService(pg), Notifier: metricEval.Notifier},
		{Src: profile.NewRegressionService(pg), Notifier: profileRegEval.Notifier},
		{Src: host.NewIncidentService(pg), Notifier: hostEval.Notifier},
		{Src: slo.NewStore(pg), Notifier: sloNotifier},
		{Src: maint, Notifier: uptimeEscNotifier},
	}
	sched := &escalation.Scheduler{
		Bindings: bindings,
		Policy:   policyStore,
		Maint:    maint,
		Pool:     pg,
		Interval: time.Duration(cfg.EscalationInterval) * time.Second,
		Now:      time.Now,
		// Dep — B5-гейт c D3-хуком (DepGate): host-инцидент, подавленный
		// планировщиком, тем же вызовом попадает в состав группы.
		Dep:         &incidentgroup.DepGate{Dep: dep, Grouper: grouper},
		SettleGrace: settleGrace,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_escalation_scheduler_last_tick_timestamp_seconds",
		"Unix time of the last completed escalation scheduler pass. Stale value means escalation ladders are not advancing for any of the six incident sources.",
		nil, sched.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_escalation_scheduler_tick_duration_seconds",
		"Duration of the last escalation scheduler pass. Approaching the interval means the scheduler stops keeping up.",
		nil, sched.LastTickSeconds)
	go sched.Run(ctx)

	// Чистка incident_escalations (M-6): без ретенции лог эскалаций растёт
	// бесконечно. Привязана к тому же сроку, что и сами инциденты
	// (GOTCHA_INCIDENT_RETENTION_DAYS) — тот же образец, что outboxJanitor:641
	// для GOTCHA_OUTBOX_RETENTION_DAYS. 0 означает «хранить вечно» (см.
	// валидацию IncidentRetentionDays >= 0) — janitor тогда не запускаем,
	// симметрично entityRetention.Any() ниже.
	if cfg.IncidentRetentionDays > 0 {
		escalationJanitor := &escalation.Janitor{
			Pool:      pg,
			Retention: time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
		}
		go escalationJanitor.Run(ctx)
	}

	// Уборка групп инцидентов (D3, §4.3): sweep вечно-открытых групп — каждый
	// тик (fail-noisy, работает независимо от ретеншена), ретеншен resolved-
	// групп — тем же сроком, что инциденты (0 — хранить вечно).
	groupJanitor := &incidentgroup.Janitor{
		Pool:      pg,
		Retention: time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
	}
	go groupJanitor.Run(ctx)
}

// detailPolicy — политика раскрытия деталей события получателю уведомления.
// Одна на процесс и собирается из конфига: раньше каждый нотифаер получал
// голый bool и сам решал по типу канала, и правило разъезжалось от нотифаера к
// нотифаеру (их семь). Теперь решение живёт в одном типе, а поле обязано быть
// заполнено — компилятор не даст завести восьмой нотифаер, забыв про гейт.
func detailPolicy(cfg Config) alert.DetailPolicy {
	return alert.NewDetailPolicy(cfg.BaseURL, cfg.TrustedRecipients, cfg.ExternalChannelDetails)
}

// logDetailPolicy — один раз при старте пишет, кому уйдут детали события.
//
// Правило перестало быть очевидным из одного флага: раньше «email — всегда,
// остальные — по GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED», теперь решает домен
// получателя. Оператор, у которого почта на другом домене, обнаружил бы
// пропажу деталей только по отсутствию текста в письме — а из лога видно
// сразу, какой список действует и чего в нём не хватает.
//
// GOTCHA_TRUSTED_RECIPIENTS печатается в ОБЕИХ ветках, не только в default:
// разбор списка не отказывает старт ни на одном значении (невалидное имя
// просто ни с чем не совпадёт), так что лог на старте — единственная
// диагностика опечатки в списке. При GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true
// список сейчас не влияет на решение (детали уходят всем), но оператор мог
// включить/выключить этот флаг позже, не трогая сам список, и должен видеть,
// что список распознан правильно ДО этого момента, а не только когда он уже
// начал на него влиять.
func logDetailPolicy(cfg Config) {
	switch {
	case cfg.ExternalChannelDetails:
		slog.Info("alert details: sent to every recipient (GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true)",
			"trusted_recipients", cfg.TrustedRecipients)
	default:
		slog.Info("alert details: sent only to trusted recipients",
			"instance_host", cfg.BaseURL,
			"trusted_recipients", cfg.TrustedRecipients,
			"hint", "add organization mail/webhook domains via GOTCHA_TRUSTED_RECIPIENTS")
	}
}

// runEvaluators — запускать ли оценщики (регрессии производительности,
// правила по метрикам, регрессии профилей) в реплике, где уже поднят режим
// аптайма. Дефолт — true: аптайм-реплика исторически единственное место, где
// они крутятся, и GOTCHA_EVALUATORS_ENABLED нужен здесь только чтобы явно
// выключить (false), не включить. В отличие от runEvaluatorsExplicit — та
// используется в режимах без аптайма, где дефолт был бы двойной оценкой при
// совместном web+uptime развёртывании, поэтому там нужно явное true.
// evaluatorsDisabledWarning — предупреждение при старте без ни явного, ни
// подразумеваемого GOTCHA_EVALUATORS_ENABLED (см. вызывающий код). Вынесена в
// константу ради теста без похода в run() целиком (тот же приём, что
// applyMigrations/heartbeatCronSnippet — тестируемость без реального
// процесса).
//
// Называет ВСЕ ШЕСТЬ циклов, которые гейтит GOTCHA_EVALUATORS_ENABLED, не четыре
// (W3-D, запись 8 = находка W3-C): startEvaluators поднимает
// metric/trace(perf)/profile/host-оценщики, а ЕЩЁ slo.Evaluator и
// escalation.Scheduler — раньше их не называли, и при раздельном
// развёртывании web+ingest без GOTCHA_EVALUATORS_ENABLED SLO-алерты сжигания
// бюджета и эскалация всех пяти прочих источников инцидентов молчали так же,
// как правило по метрике, а сама строка предупреждения об этом не говорила —
// тот же класс дефекта, который она призвана объявлять.
const evaluatorsDisabledWarning = "metric/performance/profile/host/slo evaluators and the escalation scheduler are NOT running in this mode; " +
	"metric alert rules, regression detection, host thresholds (disk/memory/load/silence), " +
	"SLO error-budget burn alerts and escalation ladders (for ALL incident sources, including uptime) " +
	"will never fire — " +
	"run a --mode=uptime (or --mode=all) replica, or set GOTCHA_EVALUATORS_ENABLED=true here"

func runEvaluators(cfg Config) bool {
	if cfg.RunEvaluators != nil {
		return *cfg.RunEvaluators
	}
	return true
}

// runEvaluatorsExplicit — включены ли оценщики ЯВНО. В режимах без аптайма
// дефолта нет: включать их самим значило бы в связке web+uptime гонять двойную
// оценку, а молча не включать — то, что уже привело к «правило включено и не
// срабатывает никогда».
func runEvaluatorsExplicit(cfg Config) bool {
	return cfg.RunEvaluators != nil && *cfg.RunEvaluators
}
