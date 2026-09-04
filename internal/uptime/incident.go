package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// Incident — период недоступности монитора.
type Incident struct {
	ID             int64
	MonitorID      int64
	StartedAt      time.Time
	ResolvedAt     *time.Time
	Cause          string
	Regions        []string
	InMaintenance  bool
	NotifiedOpen   bool
	NotifiedClose  bool
	LastRemindedAt *time.Time
	// SuppressedByDep — инцидент подавлен, потому что у него есть упавший
	// задекларированный родитель (alert_dependencies, B5). Единственный
	// писатель — Service.MarkSuppressedByDep, вызываемый depsuppress.Suppressor
	// (source="uptime" резолвится в самом uptime, не в Suppressor — см. T7).
	SuppressedByDep bool
	// AcknowledgedAt/AcknowledgedBy — подтверждение оператором (B4-паритет,
	// W2-C находка 2, миграция 0084): те же поля, что у остальных 5
	// инцидент-таблиц (0077).
	AcknowledgedAt *time.Time
	AcknowledgedBy *int64
	// Severity/EscalationLevel/LastEscalatedAt — лесенка эскалации (B4,
	// миграция 0084 добавляет их incidents наравне с остальными пятью).
	Severity        string
	EscalationLevel int
	LastEscalatedAt *time.Time
	// NotifyOpenFailed/NotifyOpenAttempts — явный признак неудачной попытки
	// доставки "down" (W2-C находка 1, миграция 0084): отделяет "пытались,
	// канал упал" от "сознательно не уведомляли" (suppressed_by_dep/
	// in_maintenance/B5-грейс) — см. Detector.notify и
	// Detector.settleHeldIncident.
	NotifyOpenFailed   bool
	NotifyOpenAttempts int
	// NotifyOpenChannels — снимок каналов, в которые Detector.notifyOpen
	// реально поставил задачу для шага 0 этого инцидента (W3-E, миграция
	// 0086, см. её докблок). nil — ретраить логирование шага 0 нечего:
	// либо оно уже завершено (успешно или принудительно после потолка
	// попыток — см. Detector.retryStepZeroLog), либо инцидент старше этой
	// колонки.
	NotifyOpenChannels []int64
}

// DeliveryExhausted сообщает, исчерпаны ли попытки доставить "down" (W2-C
// находка 1, усиление ревью аудита 2026-08-27): NotifyOpenFailed=true само
// по себе значит "ретрай ещё идёт" (settleHeldIncident продолжит пытаться на
// следующих тиках) — это отдельный, куда более тревожный случай: канал
// доставки мёртв ПО-НАСТОЯЩЕМУ (сломанный вебхук, отозванный токен), retry
// исчерпан (см. maxNotifyOpenAttempts), и без явного сигнала в интерфейсе
// открытый неподтверждённый инцидент выглядит НЕОТЛИЧИМО от обычного — ровно
// тот случай, ради которого заводилась находка 1: человек об аварии не
// узнаёт. UI (incidents.templ) показывает по этому предикату бейдж, тем же
// образом, что incident.badge.suppressed_by_dep.
func (inc Incident) DeliveryExhausted() bool {
	return inc.NotifyOpenFailed && inc.NotifyOpenAttempts >= maxNotifyOpenAttempts
}

const incidentColumns = `id, monitor_id, started_at, resolved_at, cause, regions, in_maintenance, notified_open, notified_close, last_reminded_at, suppressed_by_dep, acknowledged_at, acknowledged_by, severity, escalation_level, last_escalated_at, notify_open_failed, notify_open_attempts, notify_open_channels`

// incidentScanDest — ЕДИНСТВЕННЫЙ список приёмников под incidentColumns, в
// том же порядке. Все разборщики строк инцидента (scanIncident, через него —
// queryIncidents и постраничные выборки) берут приёмники отсюда: два
// независимых Scan-списка на одну строку колонок уже разъезжались —
// notify_open_channels (миграция 0086) доехала до incidentColumns и
// scanIncident, но не до ручного Scan, который тогда жил в queryIncidentsPaged,
// и обе постраничные выборки инцидентов начали отдавать 500 «number of field
// descriptions must equal number of destinations». Новая колонка теперь
// правится в двух местах рядом (константа и этот список), а не в трёх врозь.
func incidentScanDest(inc *Incident) []any {
	return []any{&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause,
		&inc.Regions, &inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt,
		&inc.SuppressedByDep, &inc.AcknowledgedAt, &inc.AcknowledgedBy, &inc.Severity,
		&inc.EscalationLevel, &inc.LastEscalatedAt, &inc.NotifyOpenFailed, &inc.NotifyOpenAttempts,
		&inc.NotifyOpenChannels}
}

func scanIncident(row pgx.Row) (Incident, error) {
	var inc Incident
	if err := row.Scan(incidentScanDest(&inc)...); err != nil {
		return Incident{}, err
	}
	return inc, nil
}

// OpenIncident opens a new incident for monitorID, unless one is already
// open. Race-safety relies on the partial unique index
// incidents_one_open_idx (monitor_id) WHERE resolved_at IS NULL: the INSERT
// targets that index directly, so of two concurrent callers exactly one
// INSERT succeeds and the other observes the conflict (DO NOTHING ->
// RETURNING yields no row) — no read-then-write race window. The loser then
// reads back the winner's incident and reports created=false.
func (s *Service) OpenIncident(ctx context.Context, monitorID int64, cause string, regions []string, inMaintenance bool) (Incident, bool, error) {
	if regions == nil {
		regions = []string{} // regions is NOT NULL; pgx encodes a nil slice as SQL NULL
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, cause, regions, in_maintenance)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING `+incidentColumns,
		monitorID, cause, regions, inMaintenance)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenIncidentFor(ctx, monitorID)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("uptime: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident: %w", err)
	}
	return inc, true, nil
}

// ResolveIncident closes the currently open incident for monitorID, if any.
// ok=false when there was nothing open (idempotent: a second call after the
// first resolve reports ok=false rather than erroring).
func (s *Service) ResolveIncident(ctx context.Context, monitorID int64, at time.Time) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET resolved_at = $2
		WHERE monitor_id = $1 AND resolved_at IS NULL
		RETURNING `+incidentColumns,
		monitorID, at)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: resolve incident: %w", err)
	}
	return inc, true, nil
}

// IncidentByID returns a single incident by its ID (found=false if it
// doesn't exist) — needed by escalation.StepNotifier.NotifyStep (W2-C
// находка 2, зеркало host.IncidentService.GetByID): the scheduler only knows
// incidentID, not the full Event, so the notifier reloads it here.
func (s *Service) IncidentByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM incidents WHERE id = $1", id)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: incident by id: %w", err)
	}
	return inc, true, nil
}

// OpenIncidentFor returns the currently open incident for monitorID, if any.
func (s *Service) OpenIncidentFor(ctx context.Context, monitorID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1 AND resolved_at IS NULL`, monitorID)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident for: %w", err)
	}
	return inc, true, nil
}

func queryIncidents(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) ([]Incident, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("uptime: incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("uptime: incidents: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// Incidents returns the most recent incidents across all of projectID's
// monitors, freshest first.
func (s *Service) Incidents(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			i.acknowledged_at, i.acknowledged_by, i.severity, i.escalation_level, i.last_escalated_at,
			i.notify_open_failed, i.notify_open_attempts, i.notify_open_channels
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1
		ORDER BY i.started_at DESC
		LIMIT $2`, projectID, limit)
}

// IncidentsForMonitor returns the most recent incidents for monitorID,
// freshest first.
func (s *Service) IncidentsForMonitor(ctx context.Context, monitorID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, monitorID, limit)
}

// IncidentsForMonitorsBatch — то же, что IncidentsForMonitor, но для набора
// monitorIDs одним запросом: до limit самых свежих инцидентов НА КАЖДЫЙ
// монитор (не limit суммарно на весь набор), тем же способом, каким
// IncidentsForMonitor режет по одному монитору. Публичная статус-страница
// иначе звала IncidentsForMonitor в цикле — третий и последний из трёх
// поштучных запросов на монитор, который ещё оставался в её сборке.
//
// row_number() OVER (PARTITION BY monitor_id ...) — оконная функция, а не
// LIMIT: обычный общий LIMIT после ORDER BY started_at DESC срезал бы топ-N
// по всему набору сразу, и монитор с активной историей инцидентов вытеснил
// бы из выдачи более тихие мониторы вместо того, чтобы у каждого был свой
// потолок в limit записей — именно так себя вёл бы цикл, который заменяем.
//
// Карта заполняется для ВСЕХ monitorIDs (тот же приём, что и в GetBatch/
// StatesBatch): монитор без единого инцидента присутствует с nil-слайсом, а
// не отсутствует вовсе — вызывающему не нужно отдельно проверять comma-ok.
func (s *Service) IncidentsForMonitorsBatch(ctx context.Context, monitorIDs []int64, limit int) (map[int64][]Incident, error) {
	out := make(map[int64][]Incident, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = nil
	}
	if limit <= 0 {
		return out, nil
	}
	rows, err := queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM (
			SELECT `+incidentColumns+`,
			       row_number() OVER (PARTITION BY monitor_id ORDER BY started_at DESC) AS rn
			FROM incidents
			WHERE monitor_id = ANY($1)
		) ranked
		WHERE rn <= $2
		ORDER BY monitor_id, started_at DESC`, monitorIDs, limit)
	if err != nil {
		return nil, err
	}
	for _, inc := range rows {
		out[inc.MonitorID] = append(out[inc.MonitorID], inc)
	}
	return out, nil
}

// queryIncidentsPaged runs a paged incident query as two statements — a
// bare count(*) over countFrom (the FROM/WHERE part alone) for the pager's
// total, then the page rows themselves — instead of count(*) OVER() inside
// the page query. Same trade-off as issue.Service.List (internal/issue/
// query.go, read its doc comment for the full reasoning): OVER() without
// PARTITION BY forces the planner to materialise and count EVERY matching
// row before LIMIT/OFFSET applies, so page 1 cost as much as reading the
// whole history; a separate light count lets the row query stop after LIMIT
// rows. The price is that the two statements are not one snapshot — an
// incident opened between them shifts total by one on a page boundary,
// self-correcting on the next load.
//
// An empty page returns total=0, exactly as the single-statement form did
// (no row carried the window count): the web pager (incidents.templ,
// monitor detail) reads total<=0 as "no such page, go to the first", and a
// non-zero total on an out-of-range page would change that behaviour.
//
// key is the single $1 filter argument shared by both statements (project or
// monitor id); the page query additionally takes $2 = limit, $3 = offset.
func queryIncidentsPaged(ctx context.Context, pool *pgxpool.Pool, countFrom, pageQuery string, key int64, limit, offset int) ([]Incident, int64, error) {
	var total int64
	if err := pool.QueryRow(ctx, "SELECT count(*)"+countFrom, key).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("uptime: incidents count: %w", err)
	}
	if int64(offset) >= total {
		return nil, 0, nil
	}
	out, err := queryIncidents(ctx, pool, pageQuery, key, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if len(out) == 0 {
		return nil, 0, nil
	}
	return out, total, nil
}

// IncidentsPaged returns one page (limit/offset) of projectID's incidents,
// freshest first, plus the total across all pages for the pager.
func (s *Service) IncidentsPaged(ctx context.Context, projectID int64, limit, offset int) ([]Incident, int64, error) {
	const from = `
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1`
	return queryIncidentsPaged(ctx, s.pool, from, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			i.acknowledged_at, i.acknowledged_by, i.severity, i.escalation_level, i.last_escalated_at,
			i.notify_open_failed, i.notify_open_attempts, i.notify_open_channels`+from+`
		ORDER BY i.started_at DESC
		LIMIT $2 OFFSET $3`, projectID, limit, offset)
}

// IncidentsForMonitorPaged returns one page (limit/offset) of monitorID's
// incidents, freshest first, plus the total across all pages.
func (s *Service) IncidentsForMonitorPaged(ctx context.Context, monitorID int64, limit, offset int) ([]Incident, int64, error) {
	const from = ` FROM incidents WHERE monitor_id = $1`
	return queryIncidentsPaged(ctx, s.pool, from, `
		SELECT `+incidentColumns+from+`
		ORDER BY started_at DESC
		LIMIT $2 OFFSET $3`, monitorID, limit, offset)
}

// MarkNotified records that an open/close notification was sent for an
// incident: open=true sets notified_open, otherwise notified_close.
//
// open=true also: (a) clears notify_open_failed — a successful delivery ends
// any retry sequence started by MarkNotifyOpenFailed, the flag exists only
// to drive retries while delivery hasn't gone through yet (see
// Detector.notify / Detector.settleHeldIncident); (b) raises escalation_level
// to at least 1 — this is what hands the incident off from Detector (sole
// owner of the FIRST "down" delivery, including its own B5 hold/grace/retry
// state machine) to escalation.Scheduler (owner of every ESCALATION step
// after the first) — see the long comment on OpenUnacked for why this
// handoff exists and why the two must not overlap on level 0. GREATEST, not
// a plain assignment: idempotent against a hypothetical second MarkNotified
// call on an incident already escalated past 1.
func (s *Service) MarkNotified(ctx context.Context, incidentID int64, open bool) error {
	column := "notified_close"
	extra := ""
	if open {
		column = "notified_open"
		extra = ", notify_open_failed = false, escalation_level = GREATEST(escalation_level, 1)"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET "+column+" = true"+extra+" WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkNotifyOpenFailed records a failed attempt to deliver the "down"
// notification: sets notify_open_failed and bumps notify_open_attempts (W2-C
// находка 1, миграция 0084). Unlike suppressed_by_dep/in_maintenance — which
// mean "consciously not notifying" — this means "tried and the channel
// failed", and is the signal that lets settleHeldIncident retry on the very
// next tick instead of waiting SettleGrace or requiring d.Dep != nil.
func (s *Service) MarkNotifyOpenFailed(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_failed = true, notify_open_attempts = notify_open_attempts + 1 WHERE id = $1",
		incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark notify open failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNotifyOpenChannels снимает notify_open_channels сразу после доставки
// шага 0 (W3-E, миграция 0086, см. её докблок): каналы, в которые
// Notifier.NotifyOpenStep0 реально поставил задачу, записываются НЕЗАВИСИМО
// от исхода последующего логирования (Detector.notifyOpen зовёт это ДО
// попытки LogStep) — иначе процесс, упавший между доставкой и записью
// снимка, потерял бы список безвозвратно, и ретраить лог было бы нечем.
func (s *Service) SetNotifyOpenChannels(ctx context.Context, incidentID int64, channelIDs []int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = $2 WHERE id = $1", incidentID, channelIDs)
	if err != nil {
		return fmt.Errorf("uptime: set notify open channels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearNotifyOpenChannels сбрасывает notify_open_channels в NULL — логирование
// шага 0 разрешилось (успешно или принудительно после потолка попыток, см.
// Detector.retryStepZeroLog), больше ретраить нечего.
func (s *Service) ClearNotifyOpenChannels(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = NULL WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: clear notify open channels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Acknowledge — подтверждение uptime-инцидента оператором (B4-паритет, W2-C
// находка 2): та же семантика, что у остальных 5 источников (см.
// host.IncidentService.Acknowledge), project_id проверяется через JOIN на
// monitors — у incidents нет собственной колонки project_id. ok=false —
// инцидент уже подтверждён/закрыт, или incidentID не принадлежит projectID
// (кросс-тенант) — идемпотентно, не ошибка.
func (s *Service) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND resolved_at IS NULL AND acknowledged_at IS NULL
		  AND monitor_id IN (SELECT id FROM monitors WHERE project_id = $2)
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: acknowledge incident: %w", err)
	}
	return true, nil
}

// Name — ключ источника для эскалации (B4, escalation.Source): совпадает с
// incident_source 'uptime' в incident_escalations (0077/0084).
func (s *Service) Name() string { return "uptime" }

// OpenUnacked возвращает открытые неподтверждённые uptime-инциденты —
// кандидаты на эскалацию (escalation.Source, W2-C находка 2). Зеркало
// host.IncidentService.OpenUnacked с ОДНИМ намеренным отличием:
// `escalation_level > 0`.
//
// Почему: у host/metric/trace/profile/slo ступень 0 лесенки — ЕДИНСТВЕННЫЙ
// механизм первой доставки "down" (см. host.Evaluator.notifyOpen), и весь
// B5-гейт (задержать/подавить/отправить после грейса) живёт целиком в
// escalation.Scheduler.tickOne — у эволюаторов своей копии этого гейта нет.
// У uptime.Detector, напротив, УЖЕ ЕСТЬ полноценный, отдельно оттестированный
// B5-автомат первой доставки (openIncident: держит "down", если у монитора
// задекларированный родитель; settleHeldIncident: подавляет навсегда, если
// родитель упал, иначе отправляет по истечении SettleGrace) и ретрай провала
// доставки (W2-C находка 1, Detector.notify/MarkNotifyOpenFailed) — тот же
// класс решений, что и Scheduler.tickOne, но с собственным состоянием
// (notified_open, notify_open_failed), не совпадающим с escalation_level.
//
// Если бы Scheduler ТОЖЕ видел эти инциденты на escalation_level=0, он
// применил бы СВОЙ независимый B5-гейт (incidentgroup.DepGate) к тем же
// строкам — два гейта решали бы одну и ту же задачу над одними и теми же
// данными на разных тикерах, и держащийся родитель мог получить "down"
// ДВАЖДЫ: один раз от Detector.settleHeldIncident по истечении ЕГО грейса,
// второй — от Scheduler по истечении ЕГО (Scheduler не знает про
// notified_open и не выставляет его — BumpEscalation трогает только
// escalation_level).
//
// Фильтр `escalation_level > 0` делает Detector ЕДИНСТВЕННЫМ владельцем
// первой доставки: инцидент становится видимым планировщику эскалации
// только ПОСЛЕ того, как Detector успешно отправил "down" (MarkNotified
// поднимает escalation_level до 1 — см. её докблок) — с этого момента
// escalation.Scheduler подхватывает ДАЛЬНЕЙШИЕ ступени лесенки (1, 2, ...)
// без пересечения с уже завершённой первой доставкой. group_id/
// incident_groups LEFT JOIN сдвигает точку отсчёта задержки на момент
// выхода из группы (та же логика D3, см. комментарий host); suppressed_by_dep
// исключён — подавленный B5 инцидент эскалацию дальше не получает, пока
// подавление не снято (см. dep_released_at ниже и Detector.settleHeldIncident).
//
// dep_released_at (миграция 0090, K1-4) — третий аргумент того же GREATEST,
// что уже перезапускает часы от выхода из группы: инцидент, освобождённый
// Detector.settleHeldIncident из-под подавления, начинает лесенку заново от
// момента освобождения — иначе он получил бы просроченные ступени лесенки
// каскадом. NULL для инцидента, никогда не подавлявшегося — COALESCE
// возвращает i.started_at, и GREATEST не меняет исход.
//
// m.enabled — пауза монитора глушит лесенку эскалации по его открытому
// инциденту (K2-2), симметрично IncidentsDueForReminder (watchdog.go).
// Инцидент НЕ закрывается и не резолвится через ResolveIncident: пауза
// значит «не проверяем и не будим», а не «сервис поднялся» — история
// инцидента не переписывается. При снятии паузы проверки возобновятся и
// штатно закроют инцидент, если сервис жив; иначе лесенка продолжится с
// того уровня, на котором остановилась.
func (s *Service) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, m.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at), COALESCE(i.dep_released_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.resolved_at IS NULL AND i.acknowledged_at IS NULL AND i.suppressed_by_dep = false
		  AND m.enabled
		  AND i.escalation_level > 0
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("uptime: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BumpEscalation атомарно продвигает уровень эскалации инцидента с from на
// from+1 и фиксирует last_escalated_at (B4, escalation.Source). ok=false,
// если level уже не равен from — планировщик проиграл гонку другому тику
// (идемпотентно). Зеркало host.IncidentService.BumpEscalation.
func (s *Service) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: bump escalation: %w", err)
	}
	return true, nil
}

// MarkSuppressedByDep records that incidentID was suppressed because a
// declared parent (alert_dependencies, B5) is itself down: source="uptime"
// resolution lives in this package (T7), unlike host incidents, whose sole
// writer for suppressed_by_dep is depsuppress.Suppressor.MarkSuppressed —
// see that method's doc comment for why the flag has exactly one writer per
// table.
func (s *Service) MarkSuppressedByDep(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET suppressed_by_dep = true WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark suppressed by dep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearSuppressedByDep snaps suppressed_by_dep back to false and stamps
// dep_released_at (migration 0090, K1-4, аудит перед 1.0) — the sole writer
// into false for this table's flag, symmetric to MarkSuppressedByDep (sole
// writer into true). Called by Detector.settleHeldIncident once ParentDown
// reports the declared parent has recovered. The "AND suppressed_by_dep"
// guard makes a repeated call idempotent — a second release doesn't stamp
// dep_released_at again, and RowsAffected()==0 (already cleared, by this
// call or a racing replica's) is exactly that idempotent no-op, not an
// error — ErrNotFound is for an incidentID that doesn't exist at all, which
// this UPDATE can't distinguish from "already cleared" by RowsAffected
// alone, so it doesn't try (M3, финревью волны 1 аудита перед 1.0; mirrors
// host.IncidentService.ClearSuppressed, which never inspects RowsAffected).
func (s *Service) ClearSuppressedByDep(ctx context.Context, incidentID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE incidents SET suppressed_by_dep = false, dep_released_at = now()
		WHERE id = $1 AND suppressed_by_dep`, incidentID)
	if err != nil {
		return fmt.Errorf("uptime: clear suppressed by dep: %w", err)
	}
	return nil
}
