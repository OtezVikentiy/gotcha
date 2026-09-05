package escalation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// maxLogFailureAttempts — граница попыток на "залогировать эту ступень"
// (W2-C находка 3, условие 2 ревью). Без неё устойчиво падающий LogStep
// (побитые данные, отвалившийся constraint, недоступная PG) даёт: bump не
// проходит → следующий тик берёт ТУ ЖЕ ступень → notifyStep пейджит СНОВА →
// LogStep падает СНОВА — на КАЖДОМ тике, бесконечно. Это хуже дыры, которую
// чинит находка 3: дыра молчала, а такой шторм будит дежурного всю ночь.
// После maxLogFailureAttempts неудачных подряд попыток залогировать ОДНУ И
// ТУ ЖЕ ступень bump продавливается принудительно, с громким логом (не
// молча) — прогресс важнее бесконечного шторма, тот же принцип, что уже
// применён к частичному провалу notifyStep ниже. Счётчик живёт в
// escalation_step_log_failures (миграция 0085) — переживает падение и
// перезапуск процесса, сбрасывается clearLogFailure при первом успехе или
// при принудительном бампе. Значение — то же, что у аналогичной границы в
// uptime.maxNotifyOpenAttempts (findings 1 этой же волны), для единообразия.
const maxLogFailureAttempts = 5

// SendStepIfDue шлёт ступень [level] лесенки, если её задержка от открытия
// инцидента (elapsed) уже настала, и бампает уровень эскалации. sent=true —
// бамп применился; в остальных случаях (лесенка исчерпана, ступень ещё не
// подошла по времени, ступень занята полностью, тотальный провал claim/
// notifyStep) — false, чтобы следующий тик повторил ступень целиком (либо,
// при занятой другой репликой ступени, только продвинул уровень).
//
// Claim-before-notify (аудит перед 1.0, K1-1): раньше порядок был
// notifyStep→LogStepChannels→bump — лог писался ПОСЛЕ отправки. На двух
// репликах планировщика (T8, один процесс на регион), тикающих по одному и
// тому же pool, это давало окно дубля: обе реплики читают escalation_level
// из OpenUnacked ДО того, как любая из них залогировала ступень, обе видят
// «ступень не отправлена», обе зовут notifyStep — получатель видит один и
// тот же шаг дважды. Теперь ступень СНАЧАЛА занимается (ClaimStepChannels —
// тот же INSERT … ON CONFLICT DO NOTHING по UNIQUE(source, incident,
// channel, step) миграции 0085, что раньше делал LogStep, только до, а не
// после отправки), и notifyStep зовётся только для каналов, которые
// действительно выиграла ЭТА реплика (won). Компромисс тот же, что уже
// применён в uptime.Watchdog.checkSSL (см. её докблок, "deliberate price"
// claim-before-notify для SSL-алертов): раньше провал ТОЛЬКО отправки после
// успешного лога ретраился следующим тиком, теперь ретраится провал заявки
// на ступень целиком — цена честная, но конкретная:
//  1. Две реплики: ступень уходит РОВНО один раз — гарантия та же UNIQUE
//     0085, что и раньше, только проверяется до notifyStep, а не после.
//  2. Крах процесса МЕЖДУ claim и notifyStep (окно — миллисекунды): claim
//     уже закоммичен, notifyStep не успел выполниться — ступень для этих
//     каналов будет ПРОПУЩЕНА, не повторена: следующий тик увидит занятую
//     ступень (won=∅) и просто продвинет уровень дальше. Раньше в этом же
//     окне (между notifyStep и логом) был ДУБЛЬ, не пропуск — обмен
//     осознанный: пропуск одной ступени лесенки на живом процессе тише
//     дубля пейджа при штатной работе двух реплик.
//  3. Провал ReleaseStepChannels (после частичного/тотального провала
//     notifyStep) — тот же класс: занятые, но не отправленные каналы
//     останутся залогированными как отправленные, хотя не отправлены —
//     следующий тик их пропустит. Громкий slog.Error, не тихое глотание.
//  4. Устойчивый провал самого claim (PG недоступна) — ступень не уходит
//     ВООБЩЕ, пока PG не вернётся: err пробрасывается наверх, bump не
//     зовётся, sent=false, следующий тик повторит claim. Раньше в этой же
//     ситуации потолок maxLogFailureAttempts продавливал bump принудительно
//     после N провалов ЛОГА уже отправленной ступени (находка W2-C-3) —
//     теперь продавливать нечего: лог и есть claim, и он либо проходит
//     целиком (INSERT идемпотентен), либо ступень просто не уходит, без
//     риска рассинхронизации между "отправлено" и "залогировано". Потолок
//     и escalation_step_log_failures (0085) остаются — они защищают
//     ЕДИНСТВЕННОГО оставшегося пользователя LogStepChannels,
//     uptime.Detector.notifyOpen (шаг 0, доставляемый Detector напрямую, в
//     обход лесенки и этой функции, — см. её докблок).
//  5. Гонка двух реплик на РАЗНЫХ исходах: A выигрывает claim (won≠∅), B
//     получает won=∅ и сразу бампит уровень (CAS 0→1, случай 1 выше); если
//     у A следом ТОТАЛЬНО проваливается notifyStep, A освобождает claim и
//     возвращает sent=false — уровень уже продвинут репликой B, а ступень
//     0 никому не ушла. Тот же класс компромисса, что и случай 2: ретрай
//     этой ступени следующим тиком уже не случится, потому что уровень
//     ушёл вперёд.
//
// Инвариант «escalation_level не обгоняет incident_escalations» сохраняется
// в точности как раньше: bump по-прежнему зовётся последним, claim/log уже
// на месте к моменту, когда уровень продвигается.
func SendStepIfDue(ctx context.Context, ladder Ladder, source string, pool *pgxpool.Pool, incidentID int64, level int, elapsed time.Duration,
	notifyStep func(channelIDs []int64, step int) ([]int64, error), bump func(id int64, from int) (bool, error)) (sent bool, err error) {
	if level >= len(ladder) {
		return false, nil
	}
	if elapsed < time.Duration(ladder[level].DelayMinutes)*time.Minute {
		return false, nil
	}
	chs := ladder[level].ChannelIDs
	if len(chs) == 0 {
		// Лесенка без каналов на этой ступени (проект без alert-каналов) —
		// нечего занимать и некому слать, но эскалация не должна клинить:
		// бампим, как и раньше.
		return bump(incidentID, level)
	}
	won, err := ClaimStepChannels(ctx, pool, source, incidentID, level, chs)
	if err != nil {
		// Claim не прошёл (PG недоступна и т.п.) — ничего не шлём, следующий
		// тик повторит claim целиком.
		return false, err
	}
	if len(won) == 0 {
		// Ступень уже занята ПОЛНОСТЬЮ — другой репликой в этом же тике, или
		// этой же репликой на предыдущем тике, упавшем между claim и
		// notifyStep (см. случай 2 докблока). Не шлём, но уровень двигаем:
		// CAS bump безопасен для гонки (победит одна реплика), а неотправку
		// в этом редком окне следующий тик уже не исправит — ступень
		// считается отработанной.
		return bump(incidentID, level)
	}
	enqueued, notifyErr := notifyStep(won, level)
	// unsent — каналы, которые выиграли claim (уже залогированы), но
	// реально не встали в очередь (тотальный или частичный провал
	// notifyStep, либо notifyStep штатно вернул не все won — недоставляемый
	// канал). Их лог откатывается, чтобы следующий тик повторил именно их,
	// а не решил, что они уже обслужены.
	unsent := difference(won, enqueued)
	relErr := ReleaseStepChannels(ctx, pool, source, incidentID, level, unsent)
	if relErr != nil {
		slog.Error("escalation: claimed step not released, next tick will skip it",
			"source", source, "incident_id", incidentID, "step", level, "channels", unsent, "error", relErr)
	}
	if notifyErr != nil && len(enqueued) == 0 {
		// ТОТАЛЬНЫЙ сбой: не бампим, следующий тик повторит ступень целиком
		// (claim к тому моменту уже освобождён — см. ReleaseStepChannels
		// выше, если она сама не провалилась).
		return false, errors.Join(notifyErr, relErr)
	}
	// Либо notifyStep не ошибся, либо хотя бы один канал реально получил
	// ступень при частичном сбое — продвигаем уровень, чтобы плохой канал не
	// клинил лесенку бесконечным пере-пейджем здоровых.
	ok, bumpErr := bump(incidentID, level)
	return ok, errors.Join(notifyErr, relErr, bumpErr)
}

// ClaimStepChannels занимает ступень [step] инцидента incidentID за каналами
// chs — одним INSERT … ON CONFLICT (incident_source, incident_id,
// channel_id, step) DO NOTHING RETURNING channel_id (UNIQUE миграции 0085),
// тот же приём, что раньше делал LogStep, но ДО отправки, а не после (см.
// докблок SendStepIfDue). won — подмножество chs, которое выиграл именно
// этот вызов; пустой won при непустом chs значит, что ступень уже занята
// другой репликой (или предыдущим упавшим тиком) — вызывающий не должен
// слать этим каналам ничего.
func ClaimStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) (won []int64, err error) {
	if len(chs) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, step, channel_id)
		SELECT $1, $2, $3, unnest($4::bigint[])
		ON CONFLICT (incident_source, incident_id, channel_id, step) DO NOTHING
		RETURNING channel_id`, source, incidentID, step, chs)
	if err != nil {
		return nil, fmt.Errorf("escalation: claim step channels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch int64
		if err := rows.Scan(&ch); err != nil {
			return nil, fmt.Errorf("escalation: claim step channels scan: %w", err)
		}
		won = append(won, ch)
	}
	return won, rows.Err()
}

// ReleaseStepChannels удаляет строки лога ступени [step] инцидента
// incidentID для каналов chs — каналы были заняты ClaimStepChannels, но не
// поставлены в очередь notifyStep (провал доставки или недоставляемый
// канал): следующий тик увидит ступень свободной для НИХ и повторит. Пустой
// chs — no-op (частый случай: notifyStep штатно отправил всё, что выиграл).
func ReleaseStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) error {
	if len(chs) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, `
		DELETE FROM incident_escalations
		WHERE incident_source = $1 AND incident_id = $2 AND step = $3 AND channel_id = ANY($4::bigint[])`,
		source, incidentID, step, chs)
	if err != nil {
		return fmt.Errorf("escalation: release step channels: %w", err)
	}
	return nil
}

// difference возвращает элементы won, которых нет в enqueued, в порядке
// won — "занято claim'ом, но реально не поставлено в очередь".
func difference(won, enqueued []int64) []int64 {
	skip := make(map[int64]bool, len(enqueued))
	for _, e := range enqueued {
		skip[e] = true
	}
	var out []int64
	for _, w := range won {
		if !skip[w] {
			out = append(out, w)
		}
	}
	return out
}

// LogStepChannels логирует шаг [step] инцидента для каждого канала в chs
// (LogStep — идемпотентно, ON CONFLICT DO NOTHING, UNIQUE(incident_source,
// incident_id, channel_id, step), миграция 0085) и следит за повторными
// провалами через escalation_step_log_failures (W2-C находка 3) — единый
// механизм для ЛЮБОГО источника, которому нужно залогировать шаг, а не
// только для лесенки внутри SendStepIfDue. Второй вызывающий — W3-E,
// uptime.Detector.notifyOpen: шаг 0 доставляет сам Detector, минуя эту
// лесенку (см. Service.OpenUnacked), но лог обязан подчиняться тому же
// потолку попыток, что и у остальных пяти источников, — не второй похожий
// механизм.
//
// done=true — логирование шага разрешилось: либо КАЖДЫЙ канал в chs
// залогирован чисто, либо maxLogFailureAttempts исчерпан и прогресс
// продавлен принудительно (громкий лог; err при этом остаётся ненулевым —
// вызывающий обязан вернуть его наверх, принудительный прогресс не значит,
// что провала не было). done=false — не всё залогировано, граница попыток
// ещё не исчерпана: вызывающий обязан повторить ИМЕННО ЭТОТ вызов на
// следующем цикле с ТЕМИ ЖЕ chs, не переотправляя доставку — правило чтения
// зависит от источника: у лесенки (SendStepIfDue) повтор естественно
// совпадает с повторной отправкой ступени, у uptime-шага-0 — нет, поэтому
// canal-список ретраится сохранённым, а не пересчитанным заново (см.
// Incident.NotifyOpenChannels, миграция 0086).
func LogStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) (done bool, err error) {
	var logErr error
	for _, ch := range chs {
		if e := LogStep(ctx, pool, source, incidentID, ch, step); e != nil {
			slog.Error("escalation: log step failed", "source", source, "incident_id", incidentID, "channel_id", ch, "step", step, "error", e)
			logErr = errors.Join(logErr, e)
		}
	}
	if logErr == nil {
		if err := clearLogFailure(ctx, pool, source, incidentID, step); err != nil {
			// Best-effort: неудача сброса не должна ронять успешный путь —
			// см. докблок clearLogFailure.
			slog.Warn("escalation: clear log failure after success failed", "source", source, "incident_id", incidentID, "step", step, "error", err)
		}
		return true, nil
	}
	attempts, trackErr := recordLogFailure(ctx, pool, source, incidentID, step)
	if trackErr != nil {
		slog.Error("escalation: record log failure failed", "source", source, "incident_id", incidentID, "step", step, "error", trackErr)
		return false, errors.Join(logErr, trackErr)
	}
	if attempts < maxLogFailureAttempts {
		return false, logErr
	}
	slog.Error("escalation: log kept failing after max attempts, forcing progress anyway",
		"source", source, "incident_id", incidentID, "step", step, "attempts", attempts, "error", logErr)
	if err := clearLogFailure(ctx, pool, source, incidentID, step); err != nil {
		slog.Error("escalation: clear log failure after forced progress failed", "source", source, "incident_id", incidentID, "step", step, "error", err)
	}
	return true, logErr
}

// RecoveryChannels возвращает каналы, в которые за время жизни инцидента
// уходила хотя бы одна ступень эскалации (несколько разных шагов могли слать
// в разные наборы каналов, поэтому DISTINCT) — recovery адресуется ИМ, а не
// всем каналам проекта заново: канал, который ни разу не видел тревогу, не
// должен первым увидеть «инцидент закрыт» (M-7 брифа Task 6).
func RecoveryChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64) ([]int64, error) {
	rows, err := pool.Query(ctx,
		"SELECT DISTINCT channel_id FROM incident_escalations WHERE incident_source = $1 AND incident_id = $2",
		source, incidentID)
	if err != nil {
		return nil, fmt.Errorf("escalation: recovery channels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var channelID int64
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("escalation: recovery channels: %w", err)
		}
		out = append(out, channelID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("escalation: recovery channels: %w", err)
	}
	return out, nil
}
