package uptime

import (
	"context"
	"log/slog"
	"time"
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
// Реализация — задача 2 этого плана.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
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

	// SettleGrace — сколько держать без уведомления открытый инцидент
	// монитора-ребёнка (Dep.HasParent=true), прежде чем всё равно отправить
	// отложенный "down", если к этому моменту падение родителя не
	// подтвердилось. Не используется, если Dep == nil.
	SettleGrace time.Duration
}

// depChecker — подмножество depsuppress.Suppressor, нужное детектору uptime
// для откладывания/подавления "down"-уведомлений монитора с задекларированным
// родителем (B5). Duck-typed локально, чтобы пакет uptime не импортировал
// depsuppress — тот же приём, что MaintenanceChecker.
type depChecker interface {
	HasParent(ctx context.Context, kind string, nodeID int64) (bool, error)
	ParentDown(ctx context.Context, kind string, nodeID int64) (bool, error)
}

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
	d.notify(ctx, inc.ID, true, downEvent(m, inc, downRegions, cause))
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
// dep-сервиса) — единственный путь, который что-то делает, это
// монитор-ребёнок, чьё "down" придержал openIncident (задекларированный
// родитель, отложенный автомат B5 T7): здесь решается, упал ли сам родитель
// (подавить навсегда) или истёк грейс отстаивания с живым родителем (отправить
// отложенный "down").
func (d *Detector) settleHeldIncident(ctx context.Context, m Monitor, inc Incident, states []State, st State, now time.Time) {
	if inc.NotifiedOpen || inc.SuppressedByDep || inc.InMaintenance || d.Dep == nil {
		// уже уведомлён / уже подавлен / подавлен окном обслуживания (B3,
		// BLOCKER-1: не воскрешать) / нет dep-сервиса — ничего не делаем.
		return
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
	case now.Sub(inc.StartedAt) >= d.SettleGrace:
		if d.Notifier == nil {
			return
		}
		downRegions := regionsWithStatus(states, "down")
		cause := causeFrom(st, states)
		d.notify(ctx, inc.ID, true, downEvent(m, inc, downRegions, cause))
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
	// !inc.NotifiedOpen — recovery ("up") is only sent to incidents whose
	// opening ("down") actually went out. This is a single reliable gate for
	// two cases where it didn't: an incident suppressed_by_dep (B5, T7) never
	// gets its "down" sent — see openIncident, which would need the same
	// dependency check duplicated here without this gate — and an incident
	// held open by notify-grace that recovers before the delayed "down" ever
	// fires. Both leave NotifiedOpen=false, and both must stay silent on
	// resolve: sending "up" with no matching "down" is a confusing recovery
	// notification for an outage the recipient was never told about.
	if !resolved || inc.InMaintenance || !inc.NotifiedOpen || d.Notifier == nil {
		return
	}

	var duration int64
	if inc.ResolvedAt != nil {
		duration = int64(inc.ResolvedAt.Sub(inc.StartedAt).Seconds())
	}
	d.notify(ctx, inc.ID, false, Event{
		Kind:            "up",
		Monitor:         m,
		Incident:        inc,
		DurationSeconds: duration,
	})
}

// notify отправляет ev через Notifier и, только при успехе, помечает
// инцидент как уведомлённый. Ошибка Notify логируется и проглатывается —
// см. комментарий OnResult.
func (d *Detector) notify(ctx context.Context, incidentID int64, open bool, ev Event) {
	if err := d.Notifier.Notify(ctx, ev); err != nil {
		slog.Error("uptime: detector: notify failed", "incident_id", incidentID, "kind", ev.Kind, "error", err)
		return
	}
	if err := d.Svc.MarkNotified(ctx, incidentID, open); err != nil {
		slog.Error("uptime: detector: mark notified failed", "incident_id", incidentID, "error", err)
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
