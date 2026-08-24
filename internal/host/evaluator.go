package host

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

const evaluatorDefaultInterval = 60 * time.Second

// freshWithin — насколько свежим должен быть last_seen хоста, чтобы
// ListActiveWithProject вообще его вернул. Хост, замолчавший дольше суток,
// выпадает из выборки — осознанно (см. комментарий Tick): расширение окна
// удвоило бы нагрузку на ClickHouse ради хостов, чей silent-инцидент и так
// уже открыт и без бампов корректно показывает «молчит».
const freshWithin = 24 * time.Hour

// aggregateWindow — окно агрегации метрик хоста (диск/память/нагрузка) на
// каждом тике.
const aggregateWindow = 5 * time.Minute

// tickBudgetShare — какую долю Interval занимает дедлайн одного тика. Меньше
// единицы намеренно: тик обязан закончиться ДО следующего, иначе зависший
// ClickHouse (его ReadTimeout по умолчанию 300с) копил бы наложенные проходы.
const tickBudgetShare = 0.8

// minTickBudget — пол бюджета тика. GOTCHA_HOST_EVAL_INTERVAL допускает
// значение в одну секунду, и доля от него дала бы 800 мс — меньше таймаута
// одного запроса в ClickHouse; на сколько-нибудь большом парке проход по
// тишине не успевал бы дойти до конца НИКОГДА, то есть частый тик отменял бы
// сам себя. Пол важнее доли: на крошечном интервале проходы просто пойдут
// реже заказанного (Run зовёт Tick последовательно, наложиться они не могут) —
// это меньшее зло, чем оценка, которая не заканчивается.
const minTickBudget = 10 * time.Second

// chQueryTimeout — потолок на ОДИН запрос в ClickHouse. Дедлайна тика мало:
// без него первый же повисший хост съедал бы весь бюджет, и соседи в этом
// тике не оценивались бы вовсе. Пять секунд против дефолтного бюджета в 48с —
// то есть повисший хост стоит примерно десятой доли тика, а не трети, как при
// прежних пятнадцати: смысл в том, чтобы «сосед всё-таки оценивается» было
// правдой, а не декларацией.
const chQueryTimeout = 5 * time.Second

// silentBumpGrowth — во сколько раз должна вырасти зафиксированная тишина,
// чтобы имело смысл обновлять инцидент (см. evalSilent).
const silentBumpGrowth = 1.5

// notifyTimeout — потолок на постановку уведомления в очередь и на пометку
// инцидента отправленным. Отдельный от бюджета тика намеренно, см. notify.
const notifyTimeout = 10 * time.Second

// Notifier — уведомление об открытии/закрытии встроенного инцидента хоста.
// Реализация — Task 12 (email/webhook/telegram через общий outbox); в тестах
// этого пакета — фейк.
//
// Ошибку возвращают оба метода (как uptime.Notifier и metric.MetricNotifier):
// по ней Evaluator.notify решает, ставить ли notified_open/notified_close.
// Без неё флаг «уведомлён» проставлялся бы и после провала постановки в
// очередь, и оператор, разбирающий «почему не пришло письмо», видел бы
// «уведомлён» там, где в очередь не попало ничего.
type Notifier interface {
	HostIncidentOpened(ctx context.Context, in Incident, h Host, s Settings) error
	HostIncidentResolved(ctx context.Context, in Incident, h Host) error

	// NotifyStep/NotifyRecovery (B4, T7) — реролл open/recovery на лесенку
	// эскалации: Evaluator больше не зовёт HostIncidentOpened/Resolved
	// напрямую (см. notifyOpen/notifyClose), а шлёт СТУПЕНЬ лесенки и
	// адресованный recovery через них. Реализованы HostNotifier (T6).
	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error)
	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

// depChecker — проверка «у узла дерева зависимостей есть родитель» (B5:
// подавление уведомлений по дереву зависимостей). Локальный duck-typed
// интерфейс, как MaintenanceChecker (maintenance.go): пакет host не должен
// импортировать depsuppress, только знать о факте. В проде реализует
// *depsuppress.Suppressor (main.go, startEvaluators), структурно — без
// явного приведения типов.
type depChecker interface {
	HasParent(ctx context.Context, kind string, nodeID int64) (bool, error)
}

// groupHook — членство/корни групп инцидентов (D3, incidentgroup.Grouper).
// Duck-typed локально, как depChecker: пакет host не импортирует
// incidentgroup. Nil-совместим — деградированная сборка без групп ведёт
// себя как до D3.
type groupHook interface {
	Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error)
	OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error
	OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error
}

// Evaluator периодически считает диск/память/нагрузку/тишину каждого живого
// хоста и открывает/закрывает встроенные инциденты (host_incidents) — калька
// metric.Evaluator и trace.Evaluator, только источник правил не БД, а
// фиксированный набор из четырёх видов (Kinds) плюс настройки проекта
// (SettingsService).
type Evaluator struct {
	Store     *Store
	Settings  *SettingsService
	Incidents *IncidentService
	Metrics   *metric.Query
	Notifier  Notifier
	Interval  time.Duration

	// Overrides/Groups — источники host- и group-уровней каскада порогов
	// (Task 3/4, resolve.go: ThresholdResolver/Effective). Nil-совместимы
	// (см. Tick): деградированная сборка без них просто резолвит на уровне
	// project/default, не паникует — но ПРОД и тестовый хелпер (newEvaluator)
	// обязаны их заполнять, иначе оценка молча вернётся к устаревшему
	// project-only поведению.
	Overrides *HostOverrideService
	Groups    *GroupThresholdService

	// Maint — окна обслуживания проекта (B3: подавление уведомлений). Nil-
	// совместим, как Overrides/Groups: деградированная сборка без него просто
	// никогда не подавляет (inMaintenance всегда false), а не паникует. ПРОД
	// (main.go, startEvaluators) обязан его заполнять.
	Maint MaintenanceChecker

	// Dep — проверка «у хоста есть задекларированный родитель» (B5: подавление
	// уведомлений по дереву зависимостей). Локальный duck-typed интерфейс, как
	// MaintenanceChecker: пакет host не должен знать о depsuppress.Suppressor,
	// только о факте наличия родителя. Nil-совместим — деградированная сборка
	// без него считает hasParent=false везде (поведение как до B5). ПРОД
	// (main.go, startEvaluators) обязан его заполнять.
	Dep depChecker

	// IncidentGroups — группы инцидентов (D3, incidentgroup.Grouper): членство
	// свежеоткрытого инцидента, ретро-перебор при открытии silent-корня и
	// закрытие группы при закрытии корня. Порядок гейтов уведомления:
	// maintenance → dep (B5) → группа (D3) → notify. Nil-совместим, как Dep.
	// Имя поля НЕ Groups — оно занято пороговым сервисом B2
	// (Groups *GroupThresholdService, см. выше).
	IncidentGroups groupHook

	// Policy — политика эскалации (B4, T7): на открытии инцидента резолвит
	// лесенку (project, severity) и решает, какая ступень уходит сейчас (см.
	// notifyOpen). Nil-совместим, как Overrides/Groups/Maint: деградированная
	// сборка без него просто не уведомляет об открытии, а не паникует. ПРОД
	// (main.go, startEvaluators) обязан его заполнять.
	Policy *escalation.PolicyStore

	// Pool — та же PG, что под Store/Incidents: читает лог эскалации
	// incident_escalations для адресного recovery при закрытии (B4, T7, см.
	// notifyClose, escalation.RecoveryChannels). Nil-совместим — деградированная
	// сборка без него просто не уведомляет о закрытии.
	Pool *pgxpool.Pool

	// StartedAt — момент, с которого оценщик наблюдает за хостами. Тишина
	// хоста, накопленная ДО него, ему не принадлежит: пока продукт стоял
	// (рестарт, недоступность PostgreSQL, разворачивание раздела на живой
	// базе), last_seen никто не обновлял, и первый же тик открыл бы silent
	// РАЗОМ ВСЕМ хостам, разослав по инциденту в каждый канал каждого проекта.
	// Проставляется автоматически на Run/первом Tick; тесты задают его руками,
	// чтобы смоделировать давно работающий оценщик.
	StartedAt time.Time

	lastTickUnix    atomic.Int64  // unix-время последнего УСПЕШНОГО тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// Run тикает каждый Interval, пока не отменят ctx.
func (e *Evaluator) Run(ctx context.Context) {
	interval := e.interval()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	e.markStarted()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.Tick(ctx); err != nil {
				slog.Error("host evaluator: tick failed", "error", err)
			}
		}
	}
}

func (e *Evaluator) interval() time.Duration {
	if e.Interval <= 0 {
		return evaluatorDefaultInterval
	}
	return e.Interval
}

// markStarted фиксирует точку отсчёта тишины, если её не задал вызывающий.
func (e *Evaluator) markStarted() {
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now().UTC()
	}
}

// LastTickUnix — unix-время последнего успешно завершённого тика (0, если ни
// одного ещё не было). Self-метрика: «оценщик умер или отстаёт» иначе никак не
// наблюдаемо — в журнал попадают только ошибки, а молчание оценщика выглядит
// ровно как отсутствие проблем на хостах.
func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

// LastTickSeconds — длительность последнего завершённого тика в секундах.
// Растущее значение рядом с Interval означает, что оценщик перестаёт
// укладываться в свой период.
func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

// Tick — один проход по всем живым хостам (last_seen свежее freshWithin).
// Ошибки локальны хосту/порогу — slog.Warn и дальше, как metric.Evaluator:
// сбой на одном хосте (кривые данные, временная недоступность CH для его
// запроса) не должен гасить оценку соседей того же тика.
//
// Проходов ДВА, и порядок их принципиален. Тишина считается ПЕРВОЙ и целиком по
// PostgreSQL (last_seen), до единого похода в ClickHouse: раньше silent стоял в
// общей очереди вперемешку с диском/памятью/нагрузкой, и молчащий ClickHouse
// (ReadTimeout 300с на запрос) не давал тику дойти до конца — продукт переставал
// сообщать «сервер лёг» ровно в тот момент, когда за наблюдаемостью и приходят.
// Тик ограничен дедлайном (tickBudgetShare), каждый запрос в CH — своим
// (chQueryTimeout).
func (e *Evaluator) Tick(ctx context.Context) error {
	e.markStarted()
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()

	hosts, err := e.Store.ListActiveWithProject(ctx, freshWithin, MaxActiveHostsPerTick)
	if err != nil {
		return fmt.Errorf("host: evaluator tick: list active: %w", err)
	}
	if len(hosts) >= MaxActiveHostsPerTick {
		// Выборка усечена — хвостовые проекты (порядок project_id, name) не
		// оцениваются вообще. Молчать об этом нельзя: снаружи это выглядит как
		// «на тех хостах всё спокойно».
		slog.Warn("host evaluator: active host list truncated, tail projects are not evaluated",
			"limit", MaxActiveHostsPerTick)
	}
	now := time.Now().UTC()

	hostIDs := make([]int64, len(hosts))
	for i, h := range hosts {
		hostIDs[i] = h.ID
	}

	// Overrides — один батч-запрос на ВСЕ хосты тика вместо N+1 (M1 брифа
	// Task 5): у хостов одного проекта host-override отдельный per-host, но
	// сам запрос один. e.Overrides==nil (деградированная сборка) и провал
	// запроса (M-1) ведут к одному и тому же — overrides остаётся nil-картой:
	// r.Overrides[id] на nil-карте безопасно возвращает нулевое значение
	// («наследовать всё»), паники нет.
	var overrides map[int64]ThresholdOverride
	if e.Overrides != nil {
		loaded, err := e.Overrides.GetForHosts(ctx, hostIDs)
		if err != nil {
			slog.Warn("host evaluator: load overrides failed", "error", err)
		} else {
			overrides = loaded
		}
	}

	// openKinds — какие (host_id, kind) сейчас держат открытый инцидент,
	// один батч-запрос на ВЕСЬ тик (M-A ремедиации Task 5): раньше
	// evalOrCloseKind при выключенном виде звал ResolveOpenByHostKind (UPDATE)
	// для КАЖДОГО хоста на КАЖДОМ тике, даже когда закрывать нечего — на парке
	// в тысячи хостов это worst-case тысячи пустых UPDATE за тик. nil-карта
	// (провал запроса) — сознательная деградация до старого поведения:
	// evalOrCloseKind тогда снова зовёт ResolveOpenByHostKind безусловно, чтобы
	// временная недоступность PostgreSQL не оставила выключенный вид с реально
	// открытым инцидентом висеть вечно.
	openKinds, err := e.Incidents.ListOpenKindsForHosts(ctx, hostIDs)
	if err != nil {
		slog.Warn("host evaluator: load open incident kinds failed", "error", err)
		openKinds = nil
	}

	// Кеши на тик для резолвера (resolve.go): у хостов одного проекта
	// (частый случай — кластер из нескольких машин) нет смысла спрашивать
	// SettingsService/GroupThresholdService на каждый хост отдельно.
	// project.ok=false метит провал загрузки (M-2): проход 2 не повторяет
	// запрос и не удваивает Warn — ровно как раньше settingsCache отсеивал
	// хосты «непрочитанного» проекта во втором проходе.
	type project struct {
		settings Settings
		exists   bool
		ok       bool
	}
	projCache := map[int64]project{}
	groupsCache := map[int64][]GroupThreshold{}
	groupsFailed := map[int64]bool{}

	// effFor — эффективные пороги хоста по каскаду host → role/env-группа →
	// project → default (Task 4: ThresholdResolver.Effective). ok=false —
	// project-настройки этого хоста не читаются (провал залогирован здесь же,
	// один раз на проект): хост пропускается целиком, как раньше при провале
	// settingsCache.
	effFor := func(h Host) (EffectiveSettings, bool) {
		p, cached := projCache[h.ProjectID]
		if !cached {
			s, exists, err := e.Settings.GetWithExists(ctx, h.ProjectID)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("host evaluator: settings failed", "project_id", h.ProjectID, "error", err)
				}
				p = project{ok: false}
			} else {
				p = project{settings: s, exists: exists, ok: true}
			}
			projCache[h.ProjectID] = p
		}
		if !p.ok {
			return EffectiveSettings{}, false
		}

		var groups []GroupThreshold
		if e.Groups != nil {
			g, cached := groupsCache[h.ProjectID]
			switch {
			case cached:
				groups = g
			case groupsFailed[h.ProjectID]:
				// уже провалилось в этом тике — не повторяем запрос/лог.
			default:
				loaded, err := e.Groups.List(ctx, h.ProjectID)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("host evaluator: group thresholds failed", "project_id", h.ProjectID, "error", err)
					}
					groupsFailed[h.ProjectID] = true
				} else {
					groups = loaded
					groupsCache[h.ProjectID] = groups
				}
			}
		}

		r := ThresholdResolver{
			Project:       p.settings,
			ProjectExists: p.exists,
			Groups:        groups,
			Overrides:     overrides,
		}
		return r.Effective(h), true
	}

	// Проход 1 — тишина. Только PostgreSQL: настройки/каскад проекта +
	// last_seen из уже выбранной строки.
	for i, h := range hosts {
		if ctx.Err() != nil {
			// Бюджет кончился. Выходим сразу и жалуемся ОДНОЙ строкой: раньше
			// каждый оставшийся хост давал собственный Warn на провалившемся
			// запросе, и журнал заливало тысячами одинаковых записей — в них
			// тонуло всё остальное, включая причину.
			slog.Warn("host evaluator: tick budget exhausted during silence pass",
				"skipped_hosts", len(hosts)-i, "budget", e.tickBudget())
			break
		}
		eff, ok := effFor(h)
		if !ok {
			continue
		}
		e.evalOrCloseKind(ctx, h, "silent", eff.Settings.SilentEnabled, openKinds, func() {
			e.evalSilent(ctx, h, eff.Settings, now)
		})
	}

	// Проход 2 — пороги по метрикам (ClickHouse). Кеш типов метрик живёт ровно
	// один тик: metricType не зависит от хоста, и у проекта с сотней машин
	// половина запросов тика была идентична (см. metric.TypeCache).
	if e.Metrics != nil {
		q := e.Metrics.WithTypeCache(metric.NewTypeCache())
		for i, h := range hosts {
			if ctx.Err() != nil {
				slog.Warn("host evaluator: tick budget exhausted during threshold pass",
					"skipped_hosts", len(hosts)-i, "budget", e.tickBudget())
				break
			}
			// Каскад уже в кеше: проект, чей резолв провалился в проходе 1,
			// сюда не попадает — повторять запрос незачем.
			eff, ok := effFor(h)
			if !ok {
				continue
			}
			e.evalOrCloseKind(ctx, h, "disk", eff.Settings.DiskEnabled, openKinds, func() {
				e.evalDisk(ctx, q, h, eff.Settings, now)
			})
			e.evalOrCloseKind(ctx, h, "memory", eff.Settings.MemoryEnabled, openKinds, func() {
				e.evalMemory(ctx, q, h, eff.Settings, now)
			})
			e.evalOrCloseKind(ctx, h, "load", eff.Settings.LoadEnabled, openKinds, func() {
				e.evalLoad(ctx, q, h, eff.Settings, now)
			})
		}
	}

	// Длительность публикуем всегда — упор в бюджет по ней и виден. А вот
	// отметку времени только у тика, ДОШЕДШЕГО до конца: иначе оценщик,
	// который каждый раз обрывается по дедлайну и не успевает оценить половину
	// парка, снаружи выглядел бы идеально здоровым — ровно вопреки тому, что
	// обещает описание self-метрики («последний ЗАВЕРШЁННЫЙ проход»).
	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("host evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "hosts", len(hosts))
		return nil
	}
	e.lastTickUnix.Store(time.Now().Unix())
	return nil
}

// tickBudget — дедлайн одного тика: доля интервала, но не меньше пола
// (см. tickBudgetShare/minTickBudget).
func (e *Evaluator) tickBudget() time.Duration {
	budget := time.Duration(float64(e.interval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// evalOrCloseKind — общий гейт «вид включён → оценить, иначе → закрыть уже
// открытый инцидент этого вида» для всех четырёх видов (silent/disk/memory/
// load). Используется обоими проходами Tick.
//
// M-A брифа Task 5: эффективный порог (host/group-override) может выключить
// вид точечно на одном хосте, не трогая проект целиком — раньше это делал
// только project-уровень (ResolveOpenByProjectKind, вызывался вручную со
// страницы настроек). Без этого гейта инцидент выключенного на хосте вида
// висел бы открытым вечно: ручного закрытия в интерфейсе нет.
//
// openKinds — префетч Tick (ListOpenKindsForHosts): non-nil карта позволяет
// пропустить ResolveOpenByHostKind вовсе, когда для (h.ID, kind) открытого
// инцидента заведомо нет — иначе выключенный вид на живом парке давал бы
// пустой UPDATE на КАЖДОМ тике (M-A ремедиации). nil-карта (провал префетча в
// Tick) — деградация до безусловного вызова, как до этой оптимизации.
func (e *Evaluator) evalOrCloseKind(ctx context.Context, h Host, kind string, enabled bool, openKinds map[int64]map[string]bool, eval func()) {
	if enabled {
		eval()
		return
	}
	if openKinds != nil && !openKinds[h.ID][kind] {
		return
	}
	if _, err := e.Incidents.ResolveOpenByHostKind(ctx, h.ID, kind); err != nil {
		slog.Warn("host evaluator: resolve disabled incident failed", "host_id", h.ID, "kind", kind, "error", err)
	}
}

// inMaintenance — проект сейчас в окне обслуживания (B3), для гейта open-
// notify в evalSilent/applyDecision. Ошибка проверки НЕ отменяет открытие
// инцидента: она лишь означает, что не удалось выяснить, плановые ли это
// работы, и трактуется как «не в окне» — молчать о реальном инциденте
// дороже, чем уведомить лишний раз (то же решение, что uptime.Detector.
// openIncident). Maint==nil (деградированная сборка) — тот же результат.
func (e *Evaluator) inMaintenance(ctx context.Context, projectID int64, now time.Time) bool {
	if e.Maint == nil {
		return false
	}
	v, err := e.Maint.InMaintenance(ctx, projectID, now)
	if err != nil {
		slog.Error("host evaluator: maintenance check failed, treating as not in maintenance",
			"project_id", projectID, "error", err)
		return false
	}
	return v
}

// evalSilent — тишина хоста: секунды с last_seen против SilentAfter.
//
// В ОТЛИЧИЕ от disk/memory/load — БЕЗ гистерезиса (design.md §4.4): сравнение
// прямое, не через metric.Decide с его 5%-полосой. Практических последствий
// у полосы здесь почти нет (Upsert выставляет last_seen=now(), и тишина после
// возврата хоста падает до долей секунды — далеко за любой мыслимой
// дед-зоной), но код обязан буквально соответствовать спеке, а не просто
// вести себя одинаково на типичных данных.
func (e *Evaluator) evalSilent(ctx context.Context, h Host, s Settings, now time.Time) {
	if !s.SilentEnabled {
		return
	}
	silence := now.Sub(h.LastSeen).Seconds()
	threshold := s.SilentAfter.Seconds()

	open, opened, err := e.Incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		slog.Warn("host evaluator: silent open-for failed", "host_id", h.ID, "error", err)
		return
	}

	switch {
	case !opened && silence > threshold:
		if !e.mayOpenSilent(h, s, now) {
			return
		}
		inMaint := e.inMaintenance(ctx, h.ProjectID, now)
		in, created, err := e.Incidents.Open(ctx, h.ProjectID, h.ID, "silent", silence, "", inMaint)
		if err != nil {
			slog.Warn("host evaluator: silent open failed", "host_id", h.ID, "error", err)
			return
		}
		if created {
			// D3: членство/корень решаются и для инцидента, открытого в
			// maintenance, — состав группы собирается всегда, гейтится
			// только уведомление.
			attached, grouped := e.groupGate(ctx, in, h)
			e.groupRootOpened(ctx, in, h, attached)
			if !inMaint && !grouped {
				// B5: хост с задекларированным родителем не получает синхронную
				// ступень 0 здесь — её досылает планировщик деп-подавления (T5)
				// после грейса и живой проверки родителя (тот мог и не открыть
				// инцидент, например уже в maintenance). Без родителя поведение
				// не меняется — уведомляем сразу, как раньше. Гейт общий для
				// silent (эта ветка) и disk/memory/load (applyDecision) — именно
				// silent-каскад «родитель недоступен → дети молчат» и есть
				// основной сценарий подавления.
				hasParent := false
				if e.Dep != nil {
					hp, err := e.Dep.HasParent(ctx, "host", h.ID)
					if err != nil {
						slog.Error("host evaluator: dep HasParent failed", "host_id", h.ID, "error", err)
						// fail-safe: ошибка проверки родителя не должна глушить
						// уведомление — считаем, что родителя нет и шлём сейчас,
						// лучше лишний раз уведомить, чем пропустить инцидент.
					} else {
						hasParent = hp
					}
				}
				if !hasParent {
					e.notifyOpen(ctx, in)
				}
			}
		}
	case opened && silence <= threshold:
		resolved, err := e.Incidents.Resolve(ctx, open.ID, silence)
		if err != nil {
			slog.Warn("host evaluator: silent resolve failed", "host_id", h.ID, "error", err)
			return
		}
		if resolved {
			e.groupRootClosed(ctx, open)
			e.notifyClose(ctx, open)
		}
	case opened && silence >= open.PeakValue*silentBumpGrowth:
		// Инцидент держим открытым, но НЕ переписываем на каждом тике: current
		// и peak тишины — величина производная (now − last_seen), и ежеминутный
		// UPDATE давал 1440 записей в сутки на хост ради числа, которое читатель
		// и так может вывести из started_at. Обновляем, когда тишина выросла
		// в silentBumpGrowth раз: peak остаётся честной оценкой максимума с
		// точностью до этого множителя, а записей — логарифм от прежнего числа.
		if err := e.Incidents.Bump(ctx, open.ID, silence, silence); err != nil {
			slog.Warn("host evaluator: silent bump failed", "host_id", h.ID, "error", err)
		}
	}
}

// mayOpenSilent — разрешено ли ОТКРЫВАТЬ тишину по этому хосту. Проверки
// касаются только открытия: закрытие уже открытого инцидента идёт по настоящей
// тишине (now − last_seen) без всяких поблажек — иначе рестарт продукта
// «восстанавливал» бы все реально молчащие хосты и рассылал ложные
// «инцидент закрыт».
//
//  1. Стартовый грейс: пока сам оценщик наблюдает меньше порога, накопленная
//     тишина может целиком принадлежать нашему простою, а не хосту.
//  2. Эфемерность: хост, чьё окно наблюдения (first_seen..last_seen) короче
//     порога, — это под, поднявшийся и умерший быстрее, чем порог тишины, а не
//     «замолчавший сервер». Проверка сознательно по PostgreSQL, а не «есть ли
//     точки в ClickHouse»: строка в hosts и так заводится ТОЛЬКО из приёма
//     метрик, то есть точки у неё были; зато поход в CH вернул бы тишину в
//     зависимость от ClickHouse — ровно то, от чего её отвязывает Tick.
func (e *Evaluator) mayOpenSilent(h Host, s Settings, now time.Time) bool {
	if now.Sub(e.StartedAt) <= s.SilentAfter {
		return false
	}
	return h.LastSeen.Sub(h.FirstSeen) >= s.SilentAfter
}

// warnHostQuery — Warn о неудачном запросе метрик по конкретному хосту, КРОМЕ
// случая «бюджет тика исчерпан»: про него Tick говорит одной строкой на весь
// проход, а по строке на хост превращали бы журнал в тысячи одинаковых
// записей, в которых тонет всё остальное. ctx здесь — контекст ТИКА (потолок
// одного запроса живёт внутри aggregate), поэтому его отмена означает именно
// исчерпанный бюджет, а не таймаут этого запроса: последний как раз стоит
// показать — он про конкретный хост.
func warnHostQuery(ctx context.Context, msg string, h Host, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn(msg, "host_id", h.ID, "error", err)
}

// aggregate — Aggregate с потолком времени на ОДИН запрос (chQueryTimeout,
// поверх дедлайна тика): повисший ClickHouse не должен отбирать бюджет тика у
// остальных хостов.
func (e *Evaluator) aggregate(ctx context.Context, q *metric.Query, projectID int64, name, host string, matchers []metric.LabelMatcher, agg string, now time.Time) (float64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, chQueryTimeout)
	defer cancel()
	return q.Aggregate(ctx, projectID, name, "", host, matchers, agg, now.Add(-aggregateWindow), now)
}

// evalDisk — max утилизации ФС за окно по всем mountpoint'ам хоста.
func (e *Evaluator) evalDisk(ctx context.Context, q *metric.Query, h Host, s Settings, now time.Time) {
	current, ok, err := e.aggregate(ctx, q, h.ProjectID, hostmetric.FilesystemUtilization, h.Name, nil, "max", now)
	if err != nil {
		warnHostQuery(ctx, "host evaluator: disk aggregate failed", h, err)
		return
	}
	if !ok {
		// Нет данных: открытый инцидент остаётся открытым (пропавшие данные —
		// не восстановление, за реальную тишину отвечает evalSilent), закрытый
		// не открываем.
		return
	}
	e.applyDecision(ctx, q, h, s, "disk", current, s.DiskThreshold, now)
}

// evalMemory — avg утилизации памяти за окно, ТОЛЬКО по state=used: без
// матчера в агрегат попал бы и state=free, и state=cached, и т.д. — сумма
// долей всех состояний тождественно ~1 и порог 0.90 пробивала бы память,
// которая на самом деле свободна.
func (e *Evaluator) evalMemory(ctx context.Context, q *metric.Query, h Host, s Settings, now time.Time) {
	matchers := []metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "used"}}
	current, ok, err := e.aggregate(ctx, q, h.ProjectID, hostmetric.MemoryUtilization, h.Name, matchers, "avg", now)
	if err != nil {
		warnHostQuery(ctx, "host evaluator: memory aggregate failed", h, err)
		return
	}
	if !ok {
		return
	}
	e.applyDecision(ctx, q, h, s, "memory", current, s.MemoryThreshold, now)
}

// evalLoad — load average за 5 минут, делённая на число логических ядер.
// Без числа ядер значение load само по себе ничего не говорит о загрузке
// (load=4 на 2 ядрах — перегрузка, на 32 — почти простой), поэтому при
// отсутствии метрики ядер нагрузка НЕ оценивается вообще, а не считается
// как есть.
func (e *Evaluator) evalLoad(ctx context.Context, q *metric.Query, h Host, s Settings, now time.Time) {
	load, ok, err := e.aggregate(ctx, q, h.ProjectID, hostmetric.LoadAvg5m, h.Name, nil, "avg", now)
	if err != nil {
		warnHostQuery(ctx, "host evaluator: load aggregate failed", h, err)
		return
	}
	if !ok {
		return
	}
	// system.cpu.logical.count — gauge-подобный счётчик, не монотонный
	// cumulative sum: agg "last" берёт актуальное число ядер, а не сумму/среднее
	// по окну (среднее числа ядер за 5 минут бессмысленно).
	cores, coresOK, err := e.aggregate(ctx, q, h.ProjectID, hostmetric.CPULogicalCount, h.Name, nil, "last", now)
	if err != nil {
		warnHostQuery(ctx, "host evaluator: cores aggregate failed", h, err)
		return
	}
	if !coresOK || cores <= 0 {
		return
	}
	e.applyDecision(ctx, q, h, s, "load", load/cores, s.LoadThreshold, now)
}

// applyDecision — общая логика открытия/бампа/закрытия для всех четырёх
// видов инцидента хоста: все они сравниваются с порогом компаратором "gt"
// (нарушение — значение ВЫШЕ порога), поэтому логика Decide/peak одна.
func (e *Evaluator) applyDecision(ctx context.Context, q *metric.Query, h Host, s Settings, kind string, current, threshold float64, now time.Time) {
	open, opened, err := e.Incidents.OpenFor(ctx, h.ID, kind)
	if err != nil {
		slog.Warn("host evaluator: open-for failed", "host_id", h.ID, "kind", kind, "error", err)
		return
	}

	d := metric.Decide(current, "gt", threshold, opened)
	switch {
	case d.Open:
		detail := ""
		if kind == "disk" {
			detail = e.worstMountpoint(ctx, q, h, now)
		}
		inMaint := e.inMaintenance(ctx, h.ProjectID, now)
		in, created, err := e.Incidents.Open(ctx, h.ProjectID, h.ID, kind, current, detail, inMaint)
		if err != nil {
			slog.Warn("host evaluator: open failed", "host_id", h.ID, "kind", kind, "error", err)
			return
		}
		if created {
			// D3: только членство — корнем ресурсный инцидент не становится
			// (Р3: корень — исключительно silent), groupRootOpened не зовётся.
			// Состав собирается и в maintenance, гейтится только уведомление.
			_, grouped := e.groupGate(ctx, in, h)
			if !inMaint && !grouped {
				// B5: хост с задекларированным родителем не получает синхронную
				// ступень 0 здесь — её досылает планировщик деп-подавления (T5)
				// после грейса и живой проверки родителя (тот мог и не открыть
				// инцидент, например уже в maintenance). Без родителя поведение
				// не меняется — уведомляем сразу, как раньше. Гейт общий для
				// silent (эта ветка) и disk/memory/load (applyDecision) — именно
				// silent-каскад «родитель недоступен → дети молчат» и есть
				// основной сценарий подавления.
				hasParent := false
				if e.Dep != nil {
					hp, err := e.Dep.HasParent(ctx, "host", h.ID)
					if err != nil {
						slog.Error("host evaluator: dep HasParent failed", "host_id", h.ID, "error", err)
						// fail-safe: ошибка проверки родителя не должна глушить
						// уведомление — считаем, что родителя нет и шлём сейчас,
						// лучше лишний раз уведомить, чем пропустить инцидент.
					} else {
						hasParent = hp
					}
				}
				if !hasParent {
					e.notifyOpen(ctx, in)
				}
			}
		}
	case d.Bump:
		peak := open.PeakValue
		if current > peak {
			peak = current
		}
		if err := e.Incidents.Bump(ctx, open.ID, current, peak); err != nil {
			slog.Warn("host evaluator: bump failed", "host_id", h.ID, "kind", kind, "error", err)
		}
	case d.Close:
		resolved, err := e.Incidents.Resolve(ctx, open.ID, current)
		if err != nil {
			slog.Warn("host evaluator: resolve failed", "host_id", h.ID, "kind", kind, "error", err)
			return
		}
		if resolved {
			e.notifyClose(ctx, open)
		}
	}
}

// worstMountpoint возвращает mountpoint с максимальным ПОСЛЕДНИМ значением
// утилизации ФС за окно (не средним — при открытии инцидента важно, что
// заполнено СЕЙЧАС). Ошибка запроса не мешает открытию инцидента — Detail
// просто останется пустым.
func (e *Evaluator) worstMountpoint(ctx context.Context, q *metric.Query, h Host, now time.Time) string {
	// queryCtx отдельной переменной, а не поверх ctx: warnHostQuery различает
	// «исчерпан бюджет тика» и «истёк потолок одного запроса» именно по
	// контексту ТИКА, и затенение ctx сделало бы это различие невозможным.
	queryCtx, cancel := context.WithTimeout(ctx, chQueryTimeout)
	defer cancel()
	res, err := q.SeriesGrouped(queryCtx, h.ProjectID, hostmetric.FilesystemUtilization, h.Name, hostmetric.AttrMountpoint, "max", now.Add(-aggregateWindow), now, aggregateWindow)
	if err != nil {
		warnHostQuery(ctx, "host evaluator: disk detail failed", h, err)
		return ""
	}
	best := ""
	var bestVal float64
	for _, g := range res.Groups {
		if len(g.Points) == 0 {
			continue
		}
		last := g.Points[len(g.Points)-1].V
		if best == "" || last > bestVal {
			best, bestVal = g.Key, last
		}
	}
	return best
}

// notifyOpen — реролл (B4, T7): открытие инцидента больше не шлёт сразу всем
// каналам проекта, а резолвит лесенку эскалации (project, severity) и шлёт
// РОВНО СТУПЕНЬ 0, если её задержка (обычно 0) уже настала — остальные
// ступени досылает планировщик (T8) по мере срабатывания таймеров. Severity
// хостовых инцидентов не переопределяется (в отличие от metric_alert_rules,
// T5) — всегда 'critical' (DEFAULT host_incidents.severity, 0077).
//
// Контекст ОТВЯЗАН от дедлайна тика (WithoutCancel + собственный таймаут) — та
// же необходимость, что была у старого notify: у подсистемы хостов нет
// механизма досылки по флагу, и уведомление, не поставленное в очередь из-за
// исчерпанного бюджета тика, не ушло бы НИКОГДА.
//
// Ошибка политики/уведомления НЕ должна ронять оценку — залогирована и
// оценщик идёт дальше (инцидент в базе уже открыт независимо от notify).
func (e *Evaluator) notifyOpen(ctx context.Context, in Incident) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	if e.Policy == nil || e.Notifier == nil || e.Pool == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, in.ProjectID, escalation.SeverityCritical)
	if err != nil {
		slog.Error("host evaluator: escalation policy failed", "incident_id", in.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "host", e.Pool, in.ID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return e.Notifier.NotifyStep(ctx, in.ID, chs, step) },
		func(id int64, from int) (bool, error) { return e.Incidents.BumpEscalation(ctx, id, from) })
	if err != nil {
		slog.Error("host evaluator: notify step failed", "incident_id", in.ID, "error", err)
		return
	}
	if sent {
		if err := e.Incidents.MarkNotified(ctx, in.ID, true); err != nil {
			slog.Warn("host evaluator: mark notified failed", "incident_id", in.ID, "error", err)
		}
	}
}

// notifyClose — реролл (B4, T7): закрытие инцидента шлёт recovery АДРЕСНО, в
// каналы, которые видели хотя бы одну ступень эскалации этого инцидента (лог
// incident_escalations), а не всем каналам проекта заново — канал, ни разу не
// получивший тревогу (лесенка с delay>0, ещё не дошедшая до него), не должен
// первым увидеть «инцидент закрыт» (M-7 брифа Task 6). Пустой набор каналов —
// молчание: ничего не отправлялось, отправлять «закрыт» нечего.
//
// Контекст отвязан от дедлайна тика по той же причине, что и notifyOpen.
func (e *Evaluator) notifyClose(ctx context.Context, in Incident) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	if e.Pool == nil || e.Notifier == nil {
		return
	}
	chs, err := escalation.RecoveryChannels(ctx, e.Pool, "host", in.ID)
	if err != nil {
		slog.Error("host evaluator: recovery channels failed", "incident_id", in.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := e.Notifier.NotifyRecovery(ctx, in.ID, chs); err != nil {
		slog.Error("host evaluator: notify recovery failed", "incident_id", in.ID, "error", err)
		return
	}
	if err := e.Incidents.MarkNotified(ctx, in.ID, false); err != nil {
		slog.Warn("host evaluator: mark notified failed", "incident_id", in.ID, "error", err)
	}
}

// groupGate — D3-гейт открытия: присоединяет свежесозданный инцидент к
// группе его down-корня. suppressNotify=true ТОЛЬКО под «информирующим»
// корнем (Р4, root.notified_open на момент attach) — тогда step0 не
// зовётся, информирует корень. Немой корень — attach только для состава,
// уведомление штатно. Fail-safe: ошибка → ведём себя как без D3 (шумим).
func (e *Evaluator) groupGate(ctx context.Context, in Incident, h Host) (attached, suppressNotify bool) {
	if e.IncidentGroups == nil {
		return false, false
	}
	attached, informing, err := e.IncidentGroups.Attach(ctx, "host", in.ID, "host", h.ID)
	if err != nil {
		slog.Error("host evaluator: group attach failed", "incident_id", in.ID, "error", err)
		return false, false
	}
	return attached, attached && informing
}

// groupRootOpened — silent-инцидент, НЕ присоединившийся членом (нет
// упавшего предка), — кандидат в корни собственной группы: ретро-перебор
// уже открытых инцидентов проекта (Р7). Зовётся и для инцидента, открытого
// в maintenance: немой корень собирает состав, гейт уведомлений членов
// решает notified_open (Р4).
func (e *Evaluator) groupRootOpened(ctx context.Context, in Incident, h Host, attachedAsMember bool) {
	if e.IncidentGroups == nil || in.Kind != "silent" || attachedAsMember {
		return
	}
	if err := e.IncidentGroups.OnRootOpened(ctx, "host", in.ID, "host", h.ID, h.ProjectID); err != nil {
		slog.Error("host evaluator: group root opened failed", "incident_id", in.ID, "error", err)
	}
}

// groupRootClosed — закрытие silent-инцидента закрывает его группу (Р5);
// страховка от пропущенного вызова — sweep (§4.4).
func (e *Evaluator) groupRootClosed(ctx context.Context, in Incident) {
	if e.IncidentGroups == nil || in.Kind != "silent" {
		return
	}
	if err := e.IncidentGroups.OnRootClosed(ctx, "host", in.ID); err != nil {
		slog.Error("host evaluator: group root closed failed", "incident_id", in.ID, "error", err)
	}
}
