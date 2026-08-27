package uptime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// depCounter — счётчик задекларированных детей узла (D3 Р9,
// depsuppress.Suppressor.DeclaredChildrenCount). Локальная duck-typed копия,
// как depChecker в detector.go; nil-значение законно — уведомления не
// зависят от D3.
type depCounter interface {
	DeclaredChildrenCount(ctx context.Context, kind string, nodeID int64) (int, error)
}

// OutboxNotifier — реализация Notifier поверх notify.Outbox: доставляет
// Event, ставя по одной задаче на каждый включённый канал, точно так же,
// как alert.Evaluator делает это для issue-алертов (см. evaluator.go —
// формат payload и правило пропуска email намеренно совпадают).
type OutboxNotifier struct {
	Alerts *alert.Service // для Alerts.Channels(projectID) — фолбэк, если у монитора нет своих каналов
	Uptime *Service       // для Uptime.MonitorChannelIDs(monitorID)
	Outbox *notify.Outbox

	// BaseURL — префикс для ссылки на монитор в уведомлении:
	// {BaseURL}/monitors/{id}.
	BaseURL string

	// EmailEnabled — см. alert.Evaluator.EmailEnabled: пока false,
	// email-каналы пропускаются (с warn-логом), чтобы не ставить в очередь
	// задачи, которые notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. alert.DetailPolicy). Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык уведомления выбирает оператор (класс №133–136).
	Locale i18n.Locale

	// DepCounts — источник числа задекларированных детей монитора для строки
	// «Зависимых узлов: N» в down-уведомлении (D3 Р9). nil — строки нет.
	DepCounts depCounter
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал
// монитора — если у монитора нет своих каналов, используются все
// включённые каналы проекта. Ошибка Enqueue по одному каналу не прерывает
// постановку остальных: все такие ошибки логируются и собираются через
// errors.Join в возвращаемое значение.
func (n *OutboxNotifier) Notify(ctx context.Context, ev Event) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// NotifyStep — StepNotifier для лесенки эскалации (B4, W2-C находка 2):
// шлёт ступень [step] uptime-инцидента incidentID в channelIDs и возвращает
// каналы, реально поставленные в очередь (см. escalation.SendStepIfDue) — не
// факт намерения. Инцидент+монитор перезагружаются по ID здесь: планировщик
// (escalation.Scheduler) знает только incidentID, не готовый Event — тот же
// приём, что у остальных 5 нотифаеров (см. SLOBurnNotifier.reloadEvent).
// Только "down": уровни лесенки эскалируют открытый инцидент, "up"/
// "ssl_expiring"/"reminder" идут вне лесенки (Detector.notify/reminder-сторож
// — не через escalation).
func (n *OutboxNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	inc, ok, err := n.Uptime.IncidentByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify step: load incident: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("uptime: notify step: incident %d not found", incidentID)
	}
	mon, err := n.Uptime.Get(ctx, inc.MonitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify step: load monitor: %w", err)
	}
	ev := downEvent(mon, inc, inc.Regions, inc.Cause)
	return n.dispatch(ctx, ev, channelIDs)
}

// dispatch — общая постановка одной готовой задачи в Outbox, используемая и
// синхронным Notify (channelIDs=nil, все включённые каналы), и NotifyStep
// (channelIDs — набор ступени эскалации, ContainsID-фильтр ПОСЛЕ
// Deliverable-гейта, см. escalation.ContainsID). Возвращает каналы, реально
// поставленные в очередь — NotifyStep отдаёт их SendStepIfDue для логирования
// (incident_escalations), Notify их игнорирует.
func (n *OutboxNotifier) dispatch(ctx context.Context, ev Event, channelIDs []int64) ([]int64, error) {
	own, err := n.Uptime.MonitorChannelIDs(ctx, ev.Monitor.ID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify: monitor channels: %w", err)
	}
	// Тела каналов всегда берём у alert.Service: только он держит мастер-ключ
	// и умеет расшифровать secret (и он же поканально пропускает каналы с
	// испорченным секретом, залогировав их).
	channels, err := n.Alerts.Channels(ctx, ev.Monitor.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify: project channels: %w", err)
	}
	if len(own) > 0 {
		// У монитора есть свои каналы — сужаем до них. Именно до
		// ПРИВЯЗАННЫХ, а не до «того, что удалось расшифровать»: если все
		// собственные каналы монитора отсеялись, уведомление не уходит
		// никуда, и это правильнее отката на каналы проекта — тот разослал
		// бы его ровно туда, откуда оператор монитор явно исключил.
		want := make(map[int64]struct{}, len(own))
		for _, id := range own {
			want[id] = struct{}{}
		}
		filtered := make([]alert.Channel, 0, len(own))
		for _, ch := range channels {
			if _, ok := want[ch.ID]; ok {
				filtered = append(filtered, ch)
			}
		}
		channels = filtered
	}

	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)

	url := fmt.Sprintf("%s/monitors/%d", n.BaseURL, ev.Monitor.ID)
	subject := subjectFor(ctx, ev)
	body := bodyFor(ctx, ev, url, n.depsLine(ctx, ev))

	var errs error
	var enqueued []int64
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if len(channelIDs) > 0 && !escalation.ContainsID(channelIDs, ch.ID) {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("uptime: email channel skipped, SMTP not configured",
				"monitor_id", ev.Monitor.ID, "channel_id", ch.ID)
			continue
		}

		payload := map[string]any{
			"kind":             ev.Kind,
			"monitor_id":       ev.Monitor.ID,
			"monitor_name":     ev.Monitor.Name,
			"project_id":       ev.Monitor.ProjectID,
			"regions":          ev.Regions,
			"cause":            ev.Cause,
			"duration_seconds": ev.DurationSeconds,
			"days_left":        ev.DaysLeft,
			"url":              url,
			"subject":          subject,
			"body":             body,
			"channel_kind":     ch.Kind,
			"target":           ch.Target,
		}
		// Гейт трансграничной передачи: получателю вне контура оператора
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.Details.AllowsDetails(ch) {
			payload = notify.RedactExternalPayload(ctx, payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("uptime: notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("uptime: notify: enqueue channel %d: %w", ch.ID, err))
			continue
		}
		enqueued = append(enqueued, ch.ID)
	}
	return enqueued, errs
}

// depsLine — строка «Зависимых узлов: N» для down-события монитора (D3 Р9):
// N — число задекларированных детей одного уровня (нейтральная формулировка
// MINOR-7). Пусто при N=0, ошибке или отсутствии счётчика — уведомление не
// должно зависеть от D3. Зеркало host.HostNotifier.depsLine.
func (n *OutboxNotifier) depsLine(ctx context.Context, ev Event) string {
	if n.DepCounts == nil || ev.Kind != "down" {
		return ""
	}
	cnt, err := n.DepCounts.DeclaredChildrenCount(ctx, "monitor", ev.Monitor.ID)
	if err != nil {
		slog.Warn("uptime: notify: declared children count failed", "monitor_id", ev.Monitor.ID, "error", err)
		return ""
	}
	if cnt == 0 {
		return ""
	}
	return i18n.Tf(ctx, "notify.uptime.deps_affected", "count", strconv.Itoa(cnt))
}

// subjectFor строит тему письма/сообщения по виду события из каталога i18n —
// по локали, положенной в ctx (класс №133–136: язык внешнего канала задаёт
// GOTCHA_LOCALE, см. OutboxNotifier.Locale).
func subjectFor(ctx context.Context, ev Event) string {
	name := ev.Monitor.Name
	switch ev.Kind {
	case "down":
		return i18n.Tf(ctx, "notify.uptime.subject.down", "name", name)
	case "up":
		return i18n.Tf(ctx, "notify.uptime.subject.up",
			"name", name, "duration", formatDuration(ev.DurationSeconds))
	case "ssl_expiring":
		return i18n.Tf(ctx, "notify.uptime.subject.ssl",
			"name", name, "days", strconv.Itoa(ev.DaysLeft))
	case "reminder":
		return i18n.Tf(ctx, "notify.uptime.subject.reminder",
			"name", name, "duration", formatDuration(ev.DurationSeconds))
	default:
		return i18n.Tf(ctx, "notify.uptime.subject.generic", "name", name, "kind", ev.Kind)
	}
}

// bodyFor строит человекочитаемый текст уведомления: причина, регионы,
// время — плюс ссылка на монитор. Каталог и локаль — как у subjectFor.
func bodyFor(ctx context.Context, ev Event, url, depsLine string) string {
	name := ev.Monitor.Name
	regions := strings.Join(ev.Regions, ", ")
	switch ev.Kind {
	case "down":
		return i18n.Tf(ctx, "notify.uptime.body.down",
			"name", name, "cause", ev.Cause, "regions", regions,
			"deps_line", depsLine, "url", url)
	case "up":
		return i18n.Tf(ctx, "notify.uptime.body.up",
			"name", name, "duration", formatDuration(ev.DurationSeconds), "url", url)
	case "ssl_expiring":
		return i18n.Tf(ctx, "notify.uptime.body.ssl",
			"name", name, "days", strconv.Itoa(ev.DaysLeft), "url", url)
	case "reminder":
		return i18n.Tf(ctx, "notify.uptime.body.reminder",
			"name", name, "duration", formatDuration(ev.DurationSeconds),
			"cause", ev.Cause, "regions", regions, "url", url)
	default:
		return i18n.Tf(ctx, "notify.uptime.body.generic", "name", name, "url", url)
	}
}

// formatDuration отображает секунды в компактном человекочитаемом виде:
// "45s" (< 1 минуты), "2m5s" (< 1 часа) или "1h5m" (>= 1 часа, секунды
// отбрасываются как незначимые на таком масштабе).
func formatDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	h := int64(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int64(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int64(d / time.Second)

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
