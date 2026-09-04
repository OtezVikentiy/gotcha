package uptime

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// Event — что случилось с монитором (вход для системы уведомлений, task 2).
type Event struct {
	Kind            string // "down" | "up" | "ssl_expiring" | "reminder"
	Monitor         Monitor
	Incident        Incident
	Regions         []string // затронутые регионы (для "down")
	Cause           string
	DurationSeconds int64 // для "up"
	DaysLeft        int   // для "ssl_expiring"
}

// Notifier доставляет Event во внешний мир (email/slack/webhook/...).
type Notifier interface {
	// Notify — разовая доставка события вне лесенки эскалации: ssl_expiring/
	// reminder (Watchdog) — у них нет открытого инцидента, и адресовать
	// recovery попросту некому.
	Notify(ctx context.Context, ev Event) error

	// NotifyOpenStep0 — "down" уровня 0 (Detector, свой B5 hold/grace/ретрай
	// — W2-C находка 1): не идёт через escalation.SendStepIfDue (см. её
	// комментарий про OpenUnacked), поэтому возвращает РЕАЛЬНО заенкененные
	// каналы — Detector логирует их в incident_escalations как шаг 0 сам
	// (W3-E), иначе RecoveryChannels не находил бы их для инцидента, не
	// дошедшего до эскалации уровня 1 (большинство), и адресный "up" не
	// уходил бы им никогда.
	NotifyOpenStep0(ctx context.Context, ev Event) ([]int64, error)

	// NotifyRecovery — CLOSE-уведомление в ЗАДАННЫЕ channelIDs (B4, T6; W3-E:
	// аптайм заведён в общий адресный контур recovery, как у остальных пяти
	// источников инцидентов).
	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

// Detector — детекция инцидентов по региональному консенсусу, поверх
// Service. Не знает ничего про HTTP/DNS/TCP — работает только с
// (Monitor, region, Result, State), которые ему отдаёт Runner.
type Detector struct {
	Svc      *Service
	Notifier Notifier // может быть nil — тогда только инциденты, без уведомлений

	// Dep резолвит состояние задекларированного родителя (alert_dependencies,
	// B5) для монитора. nil-безопасно: nil означает «графа зависимостей нет» —
	// поведение в точности как до B5 (openIncident всегда уведомляет
	// синхронно). Конкретная реализация — depsuppress.Suppressor; здесь
	// потребляется через локальный duck-typed интерфейс, чтобы пакет uptime
	// не импортировал depsuppress (тот же приём, что MaintenanceChecker).
	Dep depChecker

	// IncidentGroups — группы инцидентов (D3, incidentgroup.Grouper): хуки
	// корня (openIncident/resolveIncident) и членство B5-подавленных детей
	// (settleHeldIncident). См. groupHook. Nil-совместим, как Dep.
	IncidentGroups groupHook

	// SettleGrace — сколько держать без уведомления открытый инцидент
	// монитора-ребёнка (Dep.HasParent=true), прежде чем всё равно отправить
	// отложенный "down", если к этому моменту падение родителя не
	// подтвердилось. Не используется, если Dep == nil.
	SettleGrace time.Duration

	// Pool — PG для incident_escalations (W3-E): notifyOpen логирует туда
	// шаг 0 сам (см. Notifier.NotifyOpenStep0), resolveIncident читает оттуда
	// адресатов recovery (escalation.RecoveryChannels). nil — то же самое, что
	// Notifier == nil: без пула логировать/адресовать нечем, и оба этапа
	// молча пропускаются (тот же nil-совместимый приём, что у Dep/
	// IncidentGroups выше).
	Pool *pgxpool.Pool
}

// depChecker — подмножество depsuppress.Suppressor, нужное детектору uptime
// для откладывания/подавления "down"-уведомлений монитора с задекларированным
// родителем (B5). Duck-typed локально, чтобы пакет uptime не импортировал
// depsuppress — тот же приём, что MaintenanceChecker.
type depChecker interface {
	HasParent(ctx context.Context, kind string, nodeID int64) (bool, error)
	ParentDown(ctx context.Context, kind string, nodeID int64) (bool, error)

	// DownRoot — топовый упавший предок узла (тот же метод, каким
	// incidentgroup.Grouper.Roots резолвит членство, — прод держит на нём
	// ОДИН инстанс *depsuppress.Suppressor, тот же, что у host.Evaluator.Dep,
	// см. main.go). Нужен openIncident (R3b, W25): узнать ФАКТИЧЕСКИЙ
	// down-корень каскада, когда только что открывшийся инцидент монитора —
	// сам downstream-узел под уже упавшим предком, а не корень собственной
	// группы.
	DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error)
}

// groupHook — группы инцидентов (D3, incidentgroup.Grouper). Duck-typed,
// nil-совместим, как Dep. У uptime НЕТ форвард-гейта D3 (MAJOR-4): членство
// возникает только через хук MarkSuppressedByDep (settleHeldIncident).
// Открытие/закрытие инцидента монитора — событие ретро-перебора корня
// (Р7), но не обязательно СОБСТВЕННОГО: R3b (W25) резолвит фактический
// down-корень через DownRoot прежде, чем звать OnRootOpened — см.
// openIncident.
type groupHook interface {
	Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error)
	OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error
	OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error

	// RootIncident — открытый инцидент узла-корня (host_incidents kind=
	// 'silent' ИЛИ uptime incidents — резолвится по rootKind). Нужен
	// openIncident (R3b, W25): монитор мог оказаться downstream-узлом под
	// упавшим host-корнем — Detector не знает про host_incidents, но
	// incidentgroup.Grouper.RootIncident умеет то и другое.
	RootIncident(ctx context.Context, rootKind string, rootID int64) (source string, incidentID, projectID int64, notified bool, found bool, err error)
}

// maxNotifyOpenAttempts — верхняя граница попыток доставки "down" после
// первого провала (W2-C находка 1). Догоняющий ретрай идёт на КАЖДОМ
// следующем тике detectIncident, а не раз в SettleGrace — интервал проверки
// монитора обычно секунды-минуты, поэтому 5 попыток дают каналу минуты на
// восстановление после короткого сбоя (SMTP/webhook-таймаут), не пейджируя
// в мёртвый канал бесконечно. После исчерпания попыток инцидент остаётся
// NotifiedOpen=false навсегда — тот же осознанный молчаливый эффект,
// что и у suppressed_by_dep/in_maintenance: "up" тоже не уйдёт (resolveIncident
// гейтит на NotifiedOpen), потому что "down" в итоге так и не был доставлен.
const maxNotifyOpenAttempts = 5

// aggStatus — агрегированный по регионам статус монитора.
type aggStatus int

const (
	aggNone aggStatus = iota // ни один регион ещё не определился (все unknown)
	aggUp
	aggDown
)

// aggregate вычисляет агрегированный статус по политике consensus by states —
// состояния регионов монитора. Регионы в статусе "unknown" не учитываются как
// определившиеся (они ещё не набрали ни fail_threshold, ни recovery_threshold).
// Если ни один регион не определился — aggNone.
//
// wantRegions — сколько регионов у монитора НАСТРОЕНО (Monitor.RegionCount).
// Он служит знаменателем для all/majority: пока не все настроенные регионы
// прислали результат, «все регионы down» и «большинство регионов down» нельзя
// считать по одним лишь определившимся. Иначе у свежего монитора на 3 региона
// первый же упавший регион давал down==decided==1 и срабатывал КАК `all`, ТАК И
// `majority`. 0 (счёт неизвестен) — откат на прежнее поведение по decided.
func aggregate(consensus Consensus, states []State, wantRegions int) aggStatus {
	var up, down int
	for _, s := range states {
		switch s.Status {
		case "up":
			up++
		case "down":
			down++
		}
	}
	decided := up + down
	if decided == 0 {
		return aggNone
	}
	total := decided
	if wantRegions > decided {
		total = wantRegions // ещё не отчитавшиеся регионы — не down
	}
	switch consensus {
	case ConsensusAny:
		if down > 0 {
			return aggDown
		}
	case ConsensusAll:
		if down == total {
			return aggDown
		}
	default: // ConsensusMajority, и защитный дефолт на случай будущих значений
		// Fail-safe: ничью (ровно половина регионов down при чётном числе,
		// напр. 2 из 4) считаем down, а не up. Инструмент мониторинга должен
		// скорее поднять инцидент, чем молча оставить монитор зелёным, когда
		// половина флота репортит недоступность (>= вместо >).
		if down*2 >= total {
			return aggDown
		}
	}
	return aggUp
}

// Aggregate вычисляет статус монитора по политике консенсуса m.Consensus и
// его текущим региональным states — та же логика, что использует Detector
// для решения "открыть/закрыть инцидент", переиспользуемая веб-UI (список
// мониторов и страница монитора, план 4, задача 2) для отображаемого
// статуса, чтобы не дублировать consensus-логику в двух местах. Возвращает
// "up"/"down"/"unknown" ("unknown" — ни один регион ещё не набрал
// fail_threshold/recovery_threshold, тот же случай, что aggNone у
// внутреннего aggregate).
func Aggregate(m Monitor, states []State) string {
	switch aggregate(m.Consensus, states, m.RegionCount) {
	case aggUp:
		return "up"
	case aggDown:
		return "down"
	default:
		return "unknown"
	}
}

// regionsWithStatus возвращает регионы states, чей Status == status.
func regionsWithStatus(states []State, status string) []string {
	var out []string
	for _, s := range states {
		if s.Status == status {
			out = append(out, s.Region)
		}
	}
	return out
}

// causeFrom выбирает причину открытия инцидента: сперва ошибка проверки,
// вызвавшей этот OnResult (st.LastError), иначе — первая непустая ошибка
// среди упавших регионов.
func causeFrom(st State, states []State) string {
	if st.LastError != "" {
		return st.LastError
	}
	for _, s := range states {
		if s.Status == "down" && s.LastError != "" {
			return s.LastError
		}
	}
	return ""
}

// OnResult — колбэк для Runner.OnResult (та же сигнатура, без ошибки: см.
// runner.go — Runner ничего не делает с возвращаемым значением, поэтому
// Detector тоже его не возвращает, а логирует и продолжает, чтобы
// runner.OnResult = detector.OnResult подключалось напрямую, без обёртки).
//
// Ошибки похода в Service (States/OpenIncident/...) и ошибки Notifier
// логируются и не всплывают: сбой уведомления не должен ронять детекцию
// (инцидент остаётся с notified_open/notified_close = false — досылка не
// ретраится здесь, это забота будущего сторожа напоминаний), а сбой самого
// Service оставляет состояние как есть — следующий результат проверки
// пересчитает консенсус заново.
func (d *Detector) OnResult(ctx context.Context, m Monitor, region string, r Result, st State) {
	d.detectIncident(ctx, m, st)
	d.updateSSL(ctx, m, r)
}

func (d *Detector) detectIncident(ctx context.Context, m Monitor, st State) {
	states, err := d.Svc.States(ctx, m.ID)
	if err != nil {
		slog.Error("uptime: detector: states failed", "monitor_id", m.ID, "error", err)
		return
	}
	agg := aggregate(m.Consensus, states, m.RegionCount)
	if agg == aggNone {
		return
	}

	inc, open, err := d.Svc.OpenIncidentFor(ctx, m.ID)
	if err != nil {
		slog.Error("uptime: detector: open incident for failed", "monitor_id", m.ID, "error", err)
		return
	}

	now := time.Now().UTC()
	switch {
	case agg == aggDown && !open:
		d.openIncident(ctx, m, states, st, now)
	case agg == aggDown && open:
		d.settleHeldIncident(ctx, m, inc, states, st, now)
	case agg == aggUp && open:
		d.resolveIncident(ctx, m, now)
	}
}

func (d *Detector) openIncident(ctx context.Context, m Monitor, states []State, st State, now time.Time) {
	downRegions := regionsWithStatus(states, "down")
	cause := causeFrom(st, states)

	// Ошибка проверки обслуживания НЕ отменяет инцидент: она лишь означает, что
	// мы не смогли выяснить, плановые ли это работы. Раньше здесь стоял return,
	// и одно окно с поясом, который не удалось загрузить, глушило мониторинг
	// всего проекта — ровно в тот момент, когда сервис лёг. Открыть инцидент и
	// уведомить лишний раз во время плановых работ дешевле, чем промолчать при
	// настоящем падении.
	inMaintenance, err := d.Svc.InMaintenance(ctx, m.ProjectID, now)
	if err != nil {
		slog.Error("uptime: detector: in maintenance check failed, treating as not in maintenance",
			"monitor_id", m.ID, "error", err)
		inMaintenance = false
	}

	inc, created, err := d.Svc.OpenIncident(ctx, m.ID, cause, downRegions, inMaintenance)
	if err != nil {
		slog.Error("uptime: detector: open incident failed", "monitor_id", m.ID, "error", err)
		return
	}
	// D3 (R3b, W25): открытие инцидента монитора — повод для ретро-перебора
	// уже открытых инцидентов проекта (Р7), но НЕ обязательно вокруг самого
	// монитора — он мог сам оказаться downstream-узлом под уже упавшим
	// предком (host или другой монитор, задекларированным ребром
	// alert_dependencies). Раньше монитор безусловно подставлялся как
	// корень: ретро-предикат искал кандидатов с DownRoot == сам монитор и
	// не находил НИКОГО, чей фактический DownRoot вёл к настоящему топ-
	// корню, — каскад, проходящий через uptime-сторону, не докрывался.
	// Resolve сначала фактический корень через DownRoot; найден и отличен
	// от самого монитора — зовём OnRootOpened по НЕМУ. Fail-safe, как и
	// везде в детекторе: nil Dep / ошибка обхода / корень не резолвится —
	// тихий откат на монитора как на корень (поведение ДО этой правки, не
	// хуже статус-кво) — это НЕ влияет на notify() ниже, решение об
	// уведомлении принимает отдельный гейт hasParent.
	if created && d.IncidentGroups != nil {
		rootSource, rootIncidentID, rootKind, rootID := "uptime", inc.ID, "monitor", m.ID
		if d.Dep != nil {
			if rk, rid, found, err := d.Dep.DownRoot(ctx, "monitor", m.ID); err != nil {
				slog.Error("uptime: detector: down root lookup failed", "incident_id", inc.ID, "error", err)
			} else if found && (rk != "monitor" || rid != m.ID) {
				if src, incID, _, _, ok, err := d.IncidentGroups.RootIncident(ctx, rk, rid); err != nil {
					slog.Warn("uptime: detector: root incident lookup failed", "root_kind", rk, "root_id", rid, "error", err)
				} else if ok {
					rootSource, rootIncidentID, rootKind, rootID = src, incID, rk, rid
				}
			}
		}
		if err := d.IncidentGroups.OnRootOpened(ctx, rootSource, rootIncidentID, rootKind, rootID, m.ProjectID); err != nil {
			slog.Error("uptime: detector: group root opened failed", "incident_id", inc.ID, "error", err)
		}
	}
	if !created || inMaintenance || d.Notifier == nil {
		return
	}

	// A monitor with a declared parent (alert_dependencies, B5) gets its
	// "down" held back instead of sent synchronously: settleHeldIncident
	// (via detectIncident, on the next tick) decides whether to suppress it
	// for good (parent confirmed down) or send it late once SettleGrace
	// elapses with the parent still up. Fail-safe: HasParent error → treat
	// as "no parent", notify immediately, same as before B5.
	hasParent := false
	if d.Dep != nil {
		if hp, err := d.Dep.HasParent(ctx, "monitor", m.ID); err != nil {
			slog.Error("uptime: detector: dep HasParent failed", "monitor_id", m.ID, "error", err)
		} else {
			hasParent = hp
		}
	}
	if hasParent {
		return
	}
	d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
}

// downEvent строит "down"-Event, общий для синхронного пути (openIncident,
// нет задекларированного родителя) и отложенного (settleHeldIncident, B5
// T7), чтобы не дублировать сборку Regions/Cause в двух местах.
func downEvent(m Monitor, inc Incident, downRegions []string, cause string) Event {
	return Event{
		Kind:     "down",
		Monitor:  m,
		Incident: inc,
		Regions:  downRegions,
		Cause:    cause,
	}
}

// settleHeldIncident переоценивает уже открытый инцидент на каждом
// следующем «всё ещё down» тике. Не-операция для подавляющего большинства
// инцидентов (уже уведомлён, уже подавлен, открыт в окне обслуживания, нет
// dep-сервиса) — единственный путь, который что-то делает при "живом"
// B5-родителе, это монитор-ребёнок, чьё "down" придержал openIncident
// (задекларированный родитель, отложенный автомат B5 T7): здесь решается,
// упал ли сам родитель (подавить навсегда) или истёк грейс отстаивания с
// живым родителем (отправить отложенный "down"). Отдельная и более ранняя
// ветка — NotifyOpenFailed (W2-C находка 1): провалившаяся попытка доставки
// ретраится немедленно, минуя весь B5-гейт.
func (d *Detector) settleHeldIncident(ctx context.Context, m Monitor, inc Incident, states []State, st State, now time.Time) {
	if inc.NotifiedOpen {
		// Уже уведомлён — доставку повторять не за чем, но лог шага 0 мог
		// не дописаться с первой попытки (W3-E, миграция 0086) — доберём его
		// здесь же, на следующих тиках, пока инцидент открыт.
		d.retryStepZeroLog(ctx, inc)
		return
	}
	if inc.NotifyOpenFailed {
		// Попытка доставки УЖЕ БЫЛА и провалилась — это НЕ "сознательно не
		// уведомляли" (тот случай остаётся в ветке ниже: suppressed_by_dep/
		// in_maintenance/B5-грейс), а сбой канала доставки. Ретраим на
		// КАЖДОМ следующем тике, без ожидания SettleGrace и без завязки на
		// d.Dep != nil — иначе авария короче SettleGrace, или процесс без
		// поднятого dep-сервиса, не получает уведомления НИКОГДА (то, что
		// нашёл аудит). Граница попыток — maxNotifyOpenAttempts, см. её
		// докблок про то, почему не бесконечный ретрай.
		if inc.NotifyOpenAttempts >= maxNotifyOpenAttempts || d.Notifier == nil {
			return
		}
		downRegions := regionsWithStatus(states, "down")
		cause := causeFrom(st, states)
		d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
		return
	}
	if inc.InMaintenance || d.Dep == nil {
		// подавлен окном обслуживания (B3, BLOCKER-1: не воскрешать) / нет
		// dep-сервиса — ничего не делаем.
		return
	}
	if inc.SuppressedByDep {
		// K1-4 (аудит перед 1.0): раньше подавленный B5 инцидент молчал
		// навсегда — писателя в suppressed_by_dep=false не было вовсе,
		// и восстановление родителя не возобновляло эскалацию НИКОГДА, даже
		// если родитель ожил через минуту после подавления. Теперь на
		// каждом "всё ещё down" тике проверяем родителя заново; если он
		// отпустил — снимаем подавление и проваливаемся в ОБЫЧНЫЙ путь
		// settle (switch ниже): грейс проверяется по исходному
		// inc.StartedAt — подавление это поле не трогает. Подавление
		// (case down выше) срабатывает на первом же "всё ещё down" тике, БЕЗ
		// проверки SettleGrace — значит ребёнок мог не отстоять свой грейс
		// вовсе (blip родителя короче SettleGrace); уведомлять немедленно
		// значило бы пейджить ровно тот шум, который грейс должен гасить.
		down, err := d.Dep.ParentDown(ctx, "monitor", m.ID)
		if err != nil {
			slog.Warn("uptime: detector: dep ParentDown failed while releasing suppressed incident",
				"monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		if down {
			return // родитель всё ещё лежит — держим
		}
		if err := d.Svc.ClearSuppressedByDep(ctx, inc.ID); err != nil {
			slog.Warn("uptime: detector: clear suppressed by dep failed",
				"monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		slog.Info("uptime: dependency recovered, incident released", "incident_id", inc.ID, "monitor_id", m.ID)
		// Дальше — обычный путь settle (switch ниже): грейс проверяется по
		// inc.StartedAt (моменту, когда ЭТОТ инцидент открылся, до всякого
		// подавления) — если ребёнок к этому моменту уже отстоял SettleGrace
		// сам по себе, "down" уходит немедленно; если нет — держим до конца
		// исходного грейса, как любой другой свежий held-инцидент.
	}
	down, err := d.Dep.ParentDown(ctx, "monitor", m.ID)
	if err != nil {
		slog.Error("uptime: detector: dep ParentDown failed", "monitor_id", m.ID, "incident_id", inc.ID, "error", err)
		// fail-safe: не подавляем; если грейс уже истёк — уведомим ниже,
		// как если бы ParentDown вернул false.
	}
	switch {
	case down:
		if err := d.Svc.MarkSuppressedByDep(ctx, inc.ID); err != nil {
			slog.Error("uptime: detector: mark suppressed by dep failed", "monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		slog.Info("uptime: detector: incident suppressed by dependency", "monitor_id", m.ID, "incident_id", inc.ID)
		// D3: B5-подавленный uptime-ребёнок получает членство в группе
		// своего down-корня — для видимости состава (§4.2). Гейт его
		// уведомлений остаётся B5-шным навсегда (Р4: B5 строже D3).
		if d.IncidentGroups != nil {
			if _, _, err := d.IncidentGroups.Attach(ctx, "uptime", inc.ID, "monitor", m.ID); err != nil {
				slog.Error("uptime: detector: group attach failed", "incident_id", inc.ID, "error", err)
			}
		}
	case now.Sub(inc.StartedAt) >= d.SettleGrace:
		if d.Notifier == nil {
			return
		}
		downRegions := regionsWithStatus(states, "down")
		cause := causeFrom(st, states)
		d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
	default:
		// В грейсе, родитель пока жив (или ParentDown ошибся) — держим.
	}
}

func (d *Detector) resolveIncident(ctx context.Context, m Monitor, now time.Time) {
	inc, resolved, err := d.Svc.ResolveIncident(ctx, m.ID, now)
	if err != nil {
		slog.Error("uptime: detector: resolve incident failed", "monitor_id", m.ID, "error", err)
		return
	}
	// D3: закрытие инцидента монитора закрывает его группу (Р5), если она
	// была; страховка от пропуска — sweep (§4.4).
	if resolved && d.IncidentGroups != nil {
		if err := d.IncidentGroups.OnRootClosed(ctx, "uptime", inc.ID); err != nil {
			slog.Error("uptime: detector: group root closed failed", "incident_id", inc.ID, "error", err)
		}
	}
	// !inc.NotifiedOpen — recovery ("up") is only sent to incidents whose
	// opening ("down") actually went out. This is a single reliable gate for
	// two cases where it didn't: an incident suppressed_by_dep (B5, T7) never
	// gets its "down" sent — see openIncident, which would need the same
	// dependency check duplicated here without this gate — and an incident
	// held open by notify-grace that recovers before the delayed "down" ever
	// fires. Both leave NotifiedOpen=false, and both must stay silent on
	// resolve: sending "up" with no matching "down" is a confusing recovery
	// notification for an outage the recipient was never told about.
	if !resolved || inc.InMaintenance || !inc.NotifiedOpen || d.Notifier == nil || d.Pool == nil {
		return
	}

	// Последний шанс добрать лог шага 0 ПЕРЕД тем, как читать
	// RecoveryChannels ниже (W3-E, миграция 0086): если монитор ушёл в
	// recovery раньше, чем settleHeldIncident успел заметить незалогированный
	// шаг (инцидент мог ни разу не пройти через неё — "down" сразу сменился
	// "up"), это последняя точка, где его можно дописать до того, как
	// RecoveryChannels посчитает набор адресатов.
	d.retryStepZeroLog(ctx, inc)

	// W3-E (кластер 4, находка «аптайм ходит мимо адресного контура»):
	// recovery адресуется каналам, которые реально видели хотя бы одну
	// ступень эскалации этого инцидента (RecoveryChannels) — тем же приёмом,
	// что и у остальных пяти источников (host/metric/slo/profile/trace,
	// notifyClose), а не всем каналам монитора/проекта заново. Раньше "up"
	// шёл тем же путём, что и "down" уровня 0 (Notify с channelIDs=nil), и
	// канал, ни разу не увидевший тревогу (лесенка с delay>0, ещё не
	// дошедшая до него), мог первым увидеть «инцидент закрыт». Пустой набор
	// каналов — молчание: ничего не отправлялось, отправлять «закрыт» нечего
	// (M-7 брифа Task 6). Это работает только потому, что notifyOpen сама
	// логирует шаг 0 в incident_escalations (см. её докблок) — без этого
	// RecoveryChannels был бы пуст для КАЖДОГО инцидента, не дошедшего до
	// эскалации уровня 1, и "up" не уходил бы почти никогда.
	//
	// ВИДИМОЕ ИЗМЕНЕНИЕ ПОВЕДЕНИЯ: если набор каналов монитора/проекта менялся
	// после открытия инцидента (канал добавили/выключили между "down" и
	// "up"), recovery теперь уходит РОВНО туда, куда реально ушла хотя бы
	// одна ступень, а не всем ТЕКУЩИМ каналам заново. Для типичной настройки
	// (набор каналов не меняется за время жизни инцидента) видимой разницы
	// нет.
	chs, err := escalation.RecoveryChannels(ctx, d.Pool, "uptime", inc.ID)
	if err != nil {
		slog.Error("uptime: detector: recovery channels failed", "incident_id", inc.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := d.Notifier.NotifyRecovery(ctx, inc.ID, chs); err != nil {
		slog.Error("uptime: detector: notify recovery failed", "incident_id", inc.ID, "error", err)
		return
	}
	if err := d.Svc.MarkNotified(ctx, inc.ID, false); err != nil {
		slog.Error("uptime: detector: mark notified failed", "incident_id", inc.ID, "error", err)
	}
}

// notifyOpen отправляет "down"-событие уровня 0 через Notifier.
// NotifyOpenStep0 и, только при успехе, помечает инцидент уведомлённым
// (notified_open); каналы, в которые задача РЕАЛЬНО поставлена, логируются в
// incident_escalations как шаг 0 (W3-E) через escalation.LogStepChannels —
// ДАЖЕ при ошибке Notify (то же правило, что у escalation.SendStepIfDue: уже
// заенкененные каналы должны знать об этом для последующего recovery, даже
// если ДРУГИЕ каналы в этой же попытке провалились). Ошибка Notify
// логируется и проглатывается — см. комментарий OnResult.
//
// Снимок каналов (Incident.NotifyOpenChannels, миграция 0086) пишется В
// incident СРАЗУ, ДО попытки LogStepChannels: LogStepChannels может не
// закрыть логирование с первой попытки (граница попыток не исчерпана,
// см. её докблок) — снимок переживает и эту незавершённость, и крах
// процесса между доставкой и логом, и остаётся источником для
// retryStepZeroLog на следующих тиках. Очищается ТОЛЬКО когда
// LogStepChannels сообщает done=true (успех или принудительный прогресс
// после потолка попыток) — до этого момента retryStepZeroLog обязан видеть
// непустой список.
func (d *Detector) notifyOpen(ctx context.Context, incidentID int64, ev Event) {
	enqueued, err := d.Notifier.NotifyOpenStep0(ctx, ev)
	// Не задваивает запись шага 0 со Scheduler'ом: OpenUnacked фильтрует
	// escalation_level > 0, а MarkNotified ниже — единственный писатель,
	// поднимающий его до 1, синхронно с этим же вызовом — значит Scheduler
	// физически не может увидеть этот инцидент раньше, чем шаг 0 отработает
	// здесь. LogStep(...ON CONFLICT DO NOTHING) — вторая, независимая линия
	// защиты на случай повторного вызова (ретрай, гонка).
	if d.Pool != nil && len(enqueued) > 0 {
		if serr := d.Svc.SetNotifyOpenChannels(ctx, incidentID, enqueued); serr != nil {
			slog.Error("uptime: detector: set notify open channels failed", "incident_id", incidentID, "error", serr)
		}
		if done, _ := escalation.LogStepChannels(ctx, d.Pool, "uptime", incidentID, 0, enqueued); done {
			if cerr := d.Svc.ClearNotifyOpenChannels(ctx, incidentID); cerr != nil {
				slog.Error("uptime: detector: clear notify open channels failed", "incident_id", incidentID, "error", cerr)
			}
		}
		// done=false: снимок остаётся в БД как есть — retryStepZeroLog
		// (settleHeldIncident/resolveIncident) повторит попытку на следующем
		// тике теми же каналами, не переотправляя "down".
	}
	if err != nil {
		slog.Error("uptime: detector: notify failed", "incident_id", incidentID, "kind", ev.Kind, "error", err)
		// W2-C находка 1: провал доставки "down" помечается явно, отдельно от
		// notified_open — settleHeldIncident увидит NotifyOpenFailed и
		// ретраит на следующем тике, не дожидаясь SettleGrace.
		if merr := d.Svc.MarkNotifyOpenFailed(ctx, incidentID); merr != nil {
			slog.Error("uptime: detector: mark notify open failed failed", "incident_id", incidentID, "error", merr)
		}
		return
	}
	if err := d.Svc.MarkNotified(ctx, incidentID, true); err != nil {
		slog.Error("uptime: detector: mark notified failed", "incident_id", incidentID, "error", err)
	}
}

// retryStepZeroLog добирает логирование шага 0 (W3-E, миграция 0086) для
// инцидента, у которого "down" реально доставлен (notifyOpen отработала), но
// escalation.LogStepChannels не смогла закрыть лог с первой попытки —
// Incident.NotifyOpenChannels хранит ИМЕННО список каналов, которым "down"
// был поставлен, не флаг "что-то не так". Ретраится СТРОГО лог: notify не
// повторяется, "down" уже ушёл, и повторная отправка была бы дублем пейджа
// — в отличие от лесенки (SendStepIfDue), где повтор ступени естественно
// совпадает с повторной отправкой по замыслу самой лесенки. Тот же потолок
// попыток (maxLogFailureAttempts), что у остальных пяти источников — общий
// механизм, не вторая копия (см. докблок LogStepChannels). Пустой
// NotifyOpenChannels — ретраить нечего: либо лог уже завершён (успешно или
// принудительно), либо инцидент старше миграции 0086.
func (d *Detector) retryStepZeroLog(ctx context.Context, inc Incident) {
	if d.Pool == nil || len(inc.NotifyOpenChannels) == 0 {
		return
	}
	done, err := escalation.LogStepChannels(ctx, d.Pool, "uptime", inc.ID, 0, inc.NotifyOpenChannels)
	if err != nil {
		slog.Error("uptime: detector: retry step 0 log failed", "incident_id", inc.ID, "error", err)
	}
	if done {
		if cerr := d.Svc.ClearNotifyOpenChannels(ctx, inc.ID); cerr != nil {
			slog.Error("uptime: detector: clear notify open channels failed", "incident_id", inc.ID, "error", cerr)
		}
	}
}

// updateSSL записывает monitors.ssl_expires_at, если результат проверки
// принёс срок действия сертификата (только https-проверки заполняют
// r.SSLExpiresAt). Само сравнение "изменилось ли значение" и очистка
// ssl_alerted_days при более поздней дате — внутри Svc.SetSSLExpiry,
// атомарно в одном UPDATE, а не здесь: m, пришедший в OnResult, может быть
// снят с очереди раньше предыдущего SetSSLExpiry и не отражать его
// результат.
func (d *Detector) updateSSL(ctx context.Context, m Monitor, r Result) {
	if r.SSLExpiresAt == nil {
		return
	}
	if err := d.Svc.SetSSLExpiry(ctx, m.ID, *r.SSLExpiresAt); err != nil {
		slog.Error("uptime: detector: set ssl expiry failed", "monitor_id", m.ID, "error", err)
	}
}
