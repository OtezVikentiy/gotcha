package uptime

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

type Event struct {
	Kind            string // "down" | "up" | "ssl_expiring" | "reminder"
	Monitor         Monitor
	Incident        Incident
	Regions         []string // только для "down"
	Cause           string
	DurationSeconds int64 // только для "up"
	DaysLeft        int   // только для "ssl_expiring"
}

type Notifier interface {
	Notify(ctx context.Context, ev Event) error

	// OpenStep0Channels резолвит каналы шага 0 для ev, не отправляя — Detector
	// клеймит их до NotifyOpenStep0, не после.
	OpenStep0Channels(ctx context.Context, ev Event) ([]int64, error)

	// NotifyOpenStep0 шлёт только в channelIDs (уже выигранные клеймом) и
	// возвращает подмножество, реально поставленное в очередь.
	NotifyOpenStep0(ctx context.Context, ev Event, channelIDs []int64) ([]int64, error)

	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

type Detector struct {
	Svc      *Service
	Notifier Notifier // может быть nil — тогда только инциденты, без уведомлений

	Dep depChecker // nil — зависимостей нет, "down" уходит сразу, без придержки

	IncidentGroups groupHook

	SettleGrace time.Duration // без Dep не используется

	Pool *pgxpool.Pool // nil — как Notifier == nil: incident_escalations не пишется и не читается
}

type depChecker interface {
	HasParent(ctx context.Context, kind string, nodeID int64) (bool, error)
	ParentDown(ctx context.Context, kind string, nodeID int64) (bool, error)

	DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error)
}

type groupHook interface {
	Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error)
	OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error
	OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error

	RootIncident(ctx context.Context, rootKind string, rootID int64) (source string, incidentID, projectID int64, notified bool, found bool, err error)
}

const maxNotifyOpenAttempts = 5

// Минута — заведомо больше обычной отправки и заметно меньше времени
// реакции оператора на пейдж; после неё клейм шага 0 можно перезабрать.
const notifyOpenLease = time.Minute

type aggStatus int

const (
	aggNone aggStatus = iota
	aggUp
	aggDown
)

// wantRegions — сколько регионов настроено: пока не все прислали результат, «все down»
// и «большинство down» нельзя считать по одним лишь определившимся регионам.
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
		total = wantRegions
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
	default:
		// ничья (половина регионов down при чётном числе) считается down —
		// фейл-сейф вместо молчания при упавшей половине флота.
		if down*2 >= total {
			return aggDown
		}
	}
	return aggUp
}

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

func regionsWithStatus(states []State, status string) []string {
	var out []string
	for _, s := range states {
		if s.Status == status {
			out = append(out, s.Region)
		}
	}
	return out
}

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

	// ошибка проверки окна обслуживания не отменяет инцидент — иначе сбой
	// самой проверки гасит алерт при настоящем падении.
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
	if created && d.IncidentGroups != nil {
		rootSource, rootIncidentID, rootKind, rootID := "uptime", inc.ID, "monitor", m.ID
		if d.Dep != nil {
			// монитор мог оказаться downstream-узлом под уже упавшим предком —
			// резолвим фактический down-корень, а не подставляем сам монитор.
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

	hasParent := false
	if d.Dep != nil {
		if hp, err := d.Dep.HasParent(ctx, "monitor", m.ID); err != nil {
			slog.Error("uptime: detector: dep HasParent failed", "monitor_id", m.ID, "error", err)
		} else {
			hasParent = hp
		}
	}
	if hasParent {
		// "down" придерживается: settleHeldIncident решит на следующем тике,
		// подавить его насовсем или всё же отправить с опозданием.
		return
	}
	d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
}

func downEvent(m Monitor, inc Incident, downRegions []string, cause string) Event {
	return Event{
		Kind:     "down",
		Monitor:  m,
		Incident: inc,
		Regions:  downRegions,
		Cause:    cause,
	}
}

func (d *Detector) settleHeldIncident(ctx context.Context, m Monitor, inc Incident, states []State, st State, now time.Time) {
	if inc.NotifiedOpen {
		d.retryStepZeroLog(ctx, inc)
		return
	}
	if inc.NotifyOpenFailed {
		if inc.NotifyOpenAttempts >= maxNotifyOpenAttempts || d.Notifier == nil {
			return
		}
		downRegions := regionsWithStatus(states, "down")
		cause := causeFrom(st, states)
		d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
		return
	}
	if inc.InMaintenance {
		return
	}
	if d.Dep == nil {
		// Без Dep придержки не бывает — сюда доходят только конкурентный
		// клейм и клейм, повисший после смерти клеймившего процесса.
		if d.Notifier == nil {
			return
		}
		downRegions := regionsWithStatus(states, "down")
		cause := causeFrom(st, states)
		d.notifyOpen(ctx, inc.ID, downEvent(m, inc, downRegions, cause))
		return
	}
	if inc.SuppressedByDep {
		down, err := d.Dep.ParentDown(ctx, "monitor", m.ID)
		if err != nil {
			slog.Warn("uptime: detector: dep ParentDown failed while releasing suppressed incident",
				"monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		if down {
			return
		}
		if err := d.Svc.ClearSuppressedByDep(ctx, inc.ID); err != nil {
			slog.Warn("uptime: detector: clear suppressed by dep failed",
				"monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		slog.Info("uptime: dependency recovered, incident released", "incident_id", inc.ID, "monitor_id", m.ID)
	}
	down, err := d.Dep.ParentDown(ctx, "monitor", m.ID)
	if err != nil {
		slog.Error("uptime: detector: dep ParentDown failed", "monitor_id", m.ID, "incident_id", inc.ID, "error", err)
	}
	switch {
	case down:
		if err := d.Svc.MarkSuppressedByDep(ctx, inc.ID); err != nil {
			slog.Error("uptime: detector: mark suppressed by dep failed", "monitor_id", m.ID, "incident_id", inc.ID, "error", err)
			return
		}
		slog.Info("uptime: detector: incident suppressed by dependency", "monitor_id", m.ID, "incident_id", inc.ID)
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
	}
}

func (d *Detector) resolveIncident(ctx context.Context, m Monitor, now time.Time) {
	inc, resolved, err := d.Svc.ResolveIncident(ctx, m.ID, now)
	if err != nil {
		slog.Error("uptime: detector: resolve incident failed", "monitor_id", m.ID, "error", err)
		return
	}
	if resolved && d.IncidentGroups != nil {
		if err := d.IncidentGroups.OnRootClosed(ctx, "uptime", inc.ID); err != nil {
			slog.Error("uptime: detector: group root closed failed", "incident_id", inc.ID, "error", err)
		}
	}
	// recovery шлётся только инцидентам, чей "down" реально ушёл (NotifiedOpen)
	// — иначе получатель видит "исправлено" без предыстории.
	if !resolved || inc.InMaintenance || !inc.NotifiedOpen || d.Notifier == nil || d.Pool == nil {
		return
	}

	d.retryStepZeroLog(ctx, inc)

	// адресуем только каналам, реально получившим хотя бы одну ступень
	// эскалации (RecoveryChannels), а не всем каналам монитора/проекта заново.
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

// Каналы шага 0 клеймятся в incident_escalations ДО отправки, не после —
// как escalation.SendStepIfDue для остальных ступеней.
func (d *Detector) notifyOpen(ctx context.Context, incidentID int64, ev Event) {
	chs, err := d.Notifier.OpenStep0Channels(ctx, ev)
	if err != nil {
		slog.Error("uptime: detector: resolve notify open channels failed", "incident_id", incidentID, "error", err)
		if merr := d.Svc.MarkNotifyOpenFailed(ctx, incidentID); merr != nil {
			slog.Error("uptime: detector: mark notify open failed failed", "incident_id", incidentID, "error", merr)
		}
		return
	}

	won := chs
	if d.Pool != nil && len(chs) > 0 {
		won, err = escalation.ClaimStepChannelsWithLease(ctx, d.Pool, "uptime", incidentID, 0, chs, notifyOpenLease)
		if err != nil {
			slog.Error("uptime: detector: claim notify open channels failed", "incident_id", incidentID, "error", err)
			if merr := d.Svc.MarkNotifyOpenFailed(ctx, incidentID); merr != nil {
				slog.Error("uptime: detector: mark notify open failed failed", "incident_id", incidentID, "error", merr)
			}
			return
		}
		if len(won) == 0 {
			// Клейм ещё не истёк — держит его конкурентный вызов или
			// notifyOpenLease с прошлого клейма не прошёл; слать нечего.
			return
		}
	}

	enqueued, err := d.Notifier.NotifyOpenStep0(ctx, ev, won)
	if d.Pool != nil {
		var unsent []int64
		for _, ch := range won {
			if !escalation.ContainsID(enqueued, ch) {
				unsent = append(unsent, ch)
			}
		}
		if len(unsent) > 0 {
			// Каналы были заняты, но не встали в очередь — следующий тик
			// увидит их свободными и повторит именно их.
			if rerr := escalation.ReleaseStepChannels(ctx, d.Pool, "uptime", incidentID, 0, unsent); rerr != nil {
				slog.Error("uptime: detector: release notify open channels failed", "incident_id", incidentID, "channels", unsent, "error", rerr)
			}
		}
		if len(enqueued) > 0 {
			if serr := d.Svc.SetNotifyOpenChannels(ctx, incidentID, enqueued); serr != nil {
				slog.Error("uptime: detector: set notify open channels failed", "incident_id", incidentID, "error", serr)
			}
		}
	}
	if err != nil {
		slog.Error("uptime: detector: notify failed", "incident_id", incidentID, "kind", ev.Kind, "error", err)
		// провал доставки помечается отдельно от notified_open —
		// settleHeldIncident увидит NotifyOpenFailed и ретраит следующим тиком.
		if merr := d.Svc.MarkNotifyOpenFailed(ctx, incidentID); merr != nil {
			slog.Error("uptime: detector: mark notify open failed failed", "incident_id", incidentID, "error", merr)
		}
		return
	}
	if err := d.Svc.MarkNotified(ctx, incidentID, true); err != nil {
		slog.Error("uptime: detector: mark notified failed", "incident_id", incidentID, "error", err)
		// Отправка уже прошла — повторный notifyOpen не даст дубль,
		// EnqueueIdempotent в NotifyOpenStep0 отличает клейм от отправки.
		if merr := d.Svc.MarkNotifyOpenFailed(ctx, incidentID); merr != nil {
			slog.Error("uptime: detector: mark notify open failed failed", "incident_id", incidentID, "error", merr)
		}
	}
}

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

func (d *Detector) updateSSL(ctx context.Context, m Monitor, r Result) {
	if r.SSLExpiresAt == nil {
		return
	}
	if err := d.Svc.SetSSLExpiry(ctx, m.ID, *r.SSLExpiresAt); err != nil {
		slog.Error("uptime: detector: set ssl expiry failed", "monitor_id", m.ID, "error", err)
	}
}
