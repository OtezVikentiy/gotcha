package host

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

const evaluatorDefaultInterval = 60 * time.Second

const freshWithin = 24 * time.Hour

const aggregateWindow = 5 * time.Minute

const tickBudgetShare = 0.8

const minTickBudget = 10 * time.Second

const chQueryTimeout = 5 * time.Second

const silentBumpGrowth = 1.5

const notifyTimeout = 10 * time.Second

type Notifier interface {
	HostIncidentOpened(ctx context.Context, in Incident, h Host, s Settings) error
	HostIncidentResolved(ctx context.Context, in Incident, h Host) error

	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error)
	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

type depChecker interface {
	HasParent(ctx context.Context, kind string, nodeID int64) (bool, error)

	DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error)
}

type groupHook interface {
	Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error)
	OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error
	OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error

	RootIncident(ctx context.Context, rootKind string, rootID int64) (source string, incidentID, projectID int64, notified bool, found bool, err error)
}

type Evaluator struct {
	Store     *Store
	Settings  *SettingsService
	Incidents *IncidentService
	Metrics   *metric.Query
	Notifier  Notifier
	Interval  time.Duration

	Overrides *HostOverrideService
	Groups    *GroupThresholdService

	Maint MaintenanceChecker

	Dep depChecker

	IncidentGroups groupHook

	Policy *escalation.PolicyStore

	Pool *pgxpool.Pool

	StartedAt time.Time

	// cursor и skipped читаются и пишутся только из Tick, вызываемого
	// последовательно из Run — конкурентной защиты не требуют.
	cursor  hostKey
	skipped atomic.Int64

	lastTickUnix    atomic.Int64
	lastTickSeconds atomic.Uint64
}

type hostKey struct {
	ProjectID int64
	Name      string
}

func hostKeyOf(h Host) hostKey { return hostKey{ProjectID: h.ProjectID, Name: h.Name} }

func (k hostKey) less(o hostKey) bool {
	if k.ProjectID != o.ProjectID {
		return k.ProjectID < o.ProjectID
	}
	return k.Name < o.Name
}

// rotateHosts возобновляет обход сразу после cursor (ORDER BY project_id,
// name) — без этого при нехватке бюджета голодает всегда один и тот же хвост.
func rotateHosts(hosts []Host, cursor hostKey) []Host {
	if len(hosts) == 0 {
		return hosts
	}
	idx := sort.Search(len(hosts), func(i int) bool { return cursor.less(hostKeyOf(hosts[i])) })
	if idx == 0 {
		return hosts
	}
	rotated := make([]Host, 0, len(hosts))
	rotated = append(rotated, hosts[idx:]...)
	rotated = append(rotated, hosts[:idx]...)
	return rotated
}

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

func (e *Evaluator) markStarted() {
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now().UTC()
	}
}

func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

func (e *Evaluator) LastTickSkippedHosts() int64 { return e.skipped.Load() }

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
		slog.Warn("host evaluator: active host list truncated, tail projects are not evaluated",
			"limit", MaxActiveHostsPerTick)
	}
	hosts = rotateHosts(hosts, e.cursor)
	now := time.Now().UTC()

	hostIDs := make([]int64, len(hosts))
	for i, h := range hosts {
		hostIDs[i] = h.ID
	}

	var overrides map[int64]ThresholdOverride
	if e.Overrides != nil {
		loaded, err := e.Overrides.GetForHosts(ctx, hostIDs)
		if err != nil {
			slog.Warn("host evaluator: load overrides failed", "error", err)
		} else {
			overrides = loaded
		}
	}

	openKinds, err := e.Incidents.ListOpenKindsForHosts(ctx, hostIDs)
	if err != nil {
		slog.Warn("host evaluator: load open incident kinds failed", "error", err)
		openKinds = nil
	}

	type project struct {
		settings Settings
		exists   bool
		ok       bool
	}
	projCache := map[int64]project{}
	groupsCache := map[int64][]GroupThreshold{}
	groupsFailed := map[int64]bool{}

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

	// Проходов два, порядок принципиален: тишина считается первой и целиком, слить нельзя.
	silentDone := len(hosts)
	for i, h := range hosts {
		if ctx.Err() != nil {
			slog.Warn("host evaluator: tick budget exhausted during silence pass",
				"skipped_hosts", len(hosts)-i, "budget", e.tickBudget())
			silentDone = i
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

	thresholdDone := len(hosts)
	if e.Metrics != nil {
		q := e.Metrics.WithTypeCache(metric.NewTypeCache())
		// По (проект, вид метрики), не по хосту — так весь тик читает окно каждой
		// метрики проекта один раз, а не по разу на каждый его хост.
		metricCache := map[projMetricKey]hostMetricBatch{}
		for i, h := range hosts {
			if ctx.Err() != nil {
				slog.Warn("host evaluator: tick budget exhausted during threshold pass",
					"skipped_hosts", len(hosts)-i, "budget", e.tickBudget())
				thresholdDone = i
				break
			}
			eff, ok := effFor(h)
			if !ok {
				continue
			}
			e.evalOrCloseKind(ctx, h, "disk", eff.Settings.DiskEnabled, openKinds, func() {
				e.evalDisk(ctx, metricCache, q, h, eff.Settings, now)
			})
			e.evalOrCloseKind(ctx, h, "memory", eff.Settings.MemoryEnabled, openKinds, func() {
				e.evalMemory(ctx, metricCache, q, h, eff.Settings, now)
			})
			e.evalOrCloseKind(ctx, h, "load", eff.Settings.LoadEnabled, openKinds, func() {
				e.evalLoad(ctx, metricCache, q, h, eff.Settings, now)
			})
		}
	}

	// furthest — сколько хостов прошли ОБЕ врезки; следующий тик продолжает сразу за ним.
	furthest := silentDone
	if thresholdDone < furthest {
		furthest = thresholdDone
	}
	e.skipped.Store(int64(len(hosts) - furthest))
	if furthest > 0 {
		e.cursor = hostKeyOf(hosts[furthest-1])
	}

	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("host evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "hosts", len(hosts))
		return nil
	}
	e.lastTickUnix.Store(time.Now().Unix())
	return nil
}

func (e *Evaluator) tickBudget() time.Duration {
	budget := time.Duration(float64(e.interval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

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
			attached, grouped := e.groupGate(ctx, in, h)
			e.groupRootOpened(ctx, in, h, attached)
			if !inMaint && !grouped {
				hasParent := false
				if e.Dep != nil {
					hp, err := e.Dep.HasParent(ctx, "host", h.ID)
					if err != nil {
						slog.Error("host evaluator: dep HasParent failed", "host_id", h.ID, "error", err)
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
		if err := e.Incidents.Bump(ctx, open.ID, silence, silence); err != nil {
			slog.Warn("host evaluator: silent bump failed", "host_id", h.ID, "error", err)
		}
	}
}

func (e *Evaluator) mayOpenSilent(h Host, s Settings, now time.Time) bool {
	if now.Sub(e.StartedAt) <= s.SilentAfter {
		return false
	}
	return h.LastSeen.Sub(h.FirstSeen) >= s.SilentAfter
}

func warnHostQuery(ctx context.Context, msg string, h Host, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn(msg, "host_id", h.ID, "error", err)
}

// Ключ батча метрик хостов за один тик: одна и та же (проект, вид метрики) для всех хостов
// проекта на этом тике — matchers/agg/name у каждого вида фиксированы вызывающим кодом.
type projMetricKey struct {
	projectID int64
	kind      string
}

type hostMetricBatch struct {
	values map[string]float64
	ok     bool
}

// Один запрос на (проект, вид метрики) за тик вместо запроса на каждый хост (host вне ключа сортировки) —
// заодно все хосты проекта видят один снимок данных, а не свой момент по ходу прохода, как было раньше.
func (e *Evaluator) batchAggregate(ctx context.Context, cache map[projMetricKey]hostMetricBatch, q *metric.Query,
	projectID int64, kind, name string, matchers []metric.LabelMatcher, agg string, now time.Time) hostMetricBatch {

	key := projMetricKey{projectID, kind}
	if b, ok := cache[key]; ok {
		return b
	}
	qctx, cancel := context.WithTimeout(ctx, chQueryTimeout)
	defer cancel()
	values, err := q.AggregateByHost(qctx, projectID, name, "", matchers, agg, now.Add(-aggregateWindow), now)
	if err != nil && ctx.Err() == nil {
		slog.Warn("host evaluator: batch aggregate failed", "project_id", projectID, "kind", kind, "error", err)
	}
	b := hostMetricBatch{values: values, ok: err == nil}
	cache[key] = b
	return b
}

func (e *Evaluator) evalDisk(ctx context.Context, cache map[projMetricKey]hostMetricBatch, q *metric.Query, h Host, s Settings, now time.Time) {
	b := e.batchAggregate(ctx, cache, q, h.ProjectID, "disk", hostmetric.FilesystemUtilization, nil, "max", now)
	if !b.ok {
		return
	}
	current, ok := b.values[h.Name]
	if !ok {
		return
	}
	e.applyDecision(ctx, q, h, s, "disk", current, s.DiskThreshold, now)
}

func (e *Evaluator) evalMemory(ctx context.Context, cache map[projMetricKey]hostMetricBatch, q *metric.Query, h Host, s Settings, now time.Time) {
	matchers := []metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "used"}}
	b := e.batchAggregate(ctx, cache, q, h.ProjectID, "memory", hostmetric.MemoryUtilization, matchers, "avg", now)
	if !b.ok {
		return
	}
	current, ok := b.values[h.Name]
	if !ok {
		return
	}
	e.applyDecision(ctx, q, h, s, "memory", current, s.MemoryThreshold, now)
}

func (e *Evaluator) evalLoad(ctx context.Context, cache map[projMetricKey]hostMetricBatch, q *metric.Query, h Host, s Settings, now time.Time) {
	loadBatch := e.batchAggregate(ctx, cache, q, h.ProjectID, "load", hostmetric.LoadAvg5m, nil, "avg", now)
	if !loadBatch.ok {
		return
	}
	load, ok := loadBatch.values[h.Name]
	if !ok {
		return
	}
	coresBatch := e.batchAggregate(ctx, cache, q, h.ProjectID, "cores", hostmetric.CPULogicalCount, nil, "last", now)
	if !coresBatch.ok {
		return
	}
	cores, coresOK := coresBatch.values[h.Name]
	if !coresOK || cores <= 0 {
		return
	}
	e.applyDecision(ctx, q, h, s, "load", load/cores, s.LoadThreshold, now)
}

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
			_, grouped := e.groupGate(ctx, in, h)
			if !inMaint && !grouped {
				hasParent := false
				if e.Dep != nil {
					hp, err := e.Dep.HasParent(ctx, "host", h.ID)
					if err != nil {
						slog.Error("host evaluator: dep HasParent failed", "host_id", h.ID, "error", err)
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

func (e *Evaluator) worstMountpoint(ctx context.Context, q *metric.Query, h Host, now time.Time) string {
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

func (e *Evaluator) notifyOpen(ctx context.Context, in Incident) {
	// WithoutCancel: отправка не должна оборваться вместе с истёкшим бюджетом тика.
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

func (e *Evaluator) notifyClose(ctx context.Context, in Incident) {
	// WithoutCancel: отправка не должна оборваться вместе с истёкшим бюджетом тика.
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

func (e *Evaluator) groupRootOpened(ctx context.Context, in Incident, h Host, attachedAsMember bool) {
	if e.IncidentGroups == nil || in.Kind != "silent" {
		return
	}
	rootSource, rootIncidentID, rootKind, rootID := "host", in.ID, "host", h.ID
	if attachedAsMember {
		if e.Dep == nil {
			return
		}
		rk, rid, found, err := e.Dep.DownRoot(ctx, "host", h.ID)
		if err != nil {
			slog.Error("host evaluator: down root lookup failed", "incident_id", in.ID, "error", err)
			return
		}
		if !found {
			return
		}
		src, incID, _, _, ok, err := e.IncidentGroups.RootIncident(ctx, rk, rid)
		if err != nil {
			slog.Warn("host evaluator: root incident lookup failed", "root_kind", rk, "root_id", rid, "error", err)
			return
		}
		if !ok {
			return
		}
		rootSource, rootIncidentID, rootKind, rootID = src, incID, rk, rid
	}
	if err := e.IncidentGroups.OnRootOpened(ctx, rootSource, rootIncidentID, rootKind, rootID, h.ProjectID); err != nil {
		slog.Error("host evaluator: group root opened failed", "incident_id", in.ID, "error", err)
	}
}

func (e *Evaluator) groupRootClosed(ctx context.Context, in Incident) {
	if e.IncidentGroups == nil || in.Kind != "silent" {
		return
	}
	if err := e.IncidentGroups.OnRootClosed(ctx, "host", in.ID); err != nil {
		slog.Error("host evaluator: group root closed failed", "incident_id", in.ID, "error", err)
	}
}
