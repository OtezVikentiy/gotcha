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
}

// aggStatus — агрегированный по регионам статус монитора.
type aggStatus int

const (
	aggNone aggStatus = iota // ни один регион ещё не определился (все unknown)
	aggUp
	aggDown
)

// aggregate вычисляет агрегированный статус по политике consensus by
// states — все регионы монитора. Регионы в статусе "unknown" не
// учитываются (они ещё не набрали ни fail_threshold, ни
// recovery_threshold). Если ни один регион не определился — aggNone.
func aggregate(consensus Consensus, states []State) aggStatus {
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
	switch consensus {
	case ConsensusAny:
		if down > 0 {
			return aggDown
		}
	case ConsensusAll:
		if down == decided {
			return aggDown
		}
	default: // ConsensusMajority, и защитный дефолт на случай будущих значений
		// Fail-safe: ничью (ровно половина регионов down при чётном числе
		// определившихся, напр. 2 из 4) считаем down, а не up. Инструмент
		// мониторинга должен скорее поднять инцидент, чем молча оставить монитор
		// зелёным, когда половина флота репортит недоступность (>= вместо >).
		if down*2 >= decided {
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
	switch aggregate(m.Consensus, states) {
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
	agg := aggregate(m.Consensus, states)
	if agg == aggNone {
		return
	}

	_, open, err := d.Svc.OpenIncidentFor(ctx, m.ID)
	if err != nil {
		slog.Error("uptime: detector: open incident for failed", "monitor_id", m.ID, "error", err)
		return
	}

	now := time.Now().UTC()
	switch {
	case agg == aggDown && !open:
		d.openIncident(ctx, m, states, st, now)
	case agg == aggUp && open:
		d.resolveIncident(ctx, m, now)
	}
}

func (d *Detector) openIncident(ctx context.Context, m Monitor, states []State, st State, now time.Time) {
	downRegions := regionsWithStatus(states, "down")
	cause := causeFrom(st, states)

	inMaintenance, err := d.Svc.InMaintenance(ctx, m.ProjectID, now)
	if err != nil {
		slog.Error("uptime: detector: in maintenance check failed", "monitor_id", m.ID, "error", err)
		return
	}

	inc, created, err := d.Svc.OpenIncident(ctx, m.ID, cause, downRegions, inMaintenance)
	if err != nil {
		slog.Error("uptime: detector: open incident failed", "monitor_id", m.ID, "error", err)
		return
	}
	if !created || inMaintenance || d.Notifier == nil {
		return
	}
	d.notify(ctx, inc.ID, true, Event{
		Kind:     "down",
		Monitor:  m,
		Incident: inc,
		Regions:  downRegions,
		Cause:    cause,
	})
}

func (d *Detector) resolveIncident(ctx context.Context, m Monitor, now time.Time) {
	inc, resolved, err := d.Svc.ResolveIncident(ctx, m.ID, now)
	if err != nil {
		slog.Error("uptime: detector: resolve incident failed", "monitor_id", m.ID, "error", err)
		return
	}
	if !resolved || inc.InMaintenance || d.Notifier == nil {
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
