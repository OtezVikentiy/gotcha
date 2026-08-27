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
// инцидента (elapsed) уже настала, и бампает уровень эскалации. Бамп
// применяется, КРОМЕ ДВУХ случаев: тотального провала notifyStep (ошибка И
// ни один канал не заенкенился — QA P2-3: частичный сбой, когда хоть один
// канал реально ушёл в очередь, прогрессу не мешает, иначе один битый канал
// клинит лесенку бесконечным пере-пейджем здоровых; пустой enqueued БЕЗ
// ошибки — это не сбой, а просто пустая лесенка, бамп идёт как обычно), и
// провала ЛОГИРОВАНИЯ хотя бы одного заенкененного канала (W2-C находка 3 —
// см. ниже), ПОКА не исчерпана maxLogFailureAttempts. sent=true — бамп
// применился; в остальных случаях (лесенка исчерпана, ступень ещё не подошла
// по времени, тотальный провал notifyStep, провал логирования в пределах
// границы) — false, чтобы следующий тик повторил ступень целиком. Порядок
// notifyStep(enqueue)→log→bump намеренный (M-3 брифа Task 6).
//
// Логирование в incident_escalations — ЗДЕСЬ, в оркестрации, а не внутри
// notifyStep (T7-fix): изначально логировал сам нотифаер (T6), и это работало
// с продовым нотифаером, но молчало с мок-нотифаерами тестов (мок не пишет в
// БД) — RecoveryChannels не находил ничего залогированного, и recovery
// немел даже когда notifyStep реально «отправил». Здесь источник истины один
// на все 6 источников: notifyStep возвращает РЕАЛЬНО заенкенные каналы (что
// бы за ним ни стояло — outbox или тестовый мок), эта функция логирует их в
// pool — то есть лог пишется независимо от реализации notifyStep.
//
// W2-C находка 3 (аудит 2026-08-27, ревью учтено): раньше ошибка ОДИНОЧНОГО
// LogStep внутри цикла по каналам молча проглатывалась (только slog.Error),
// и bump шёл дальше независимо — дыра в incident_escalations (уровень
// продвинут, но лог не отражает реально отправленный шаг для этого канала).
// Теперь такая ошибка (ЛОГА, не доставки — частичный провал notifyStep сам
// по себе bump не блокирует, см. ветку ниже) блокирует bump, пока не
// исчерпана maxLogFailureAttempts.
//
// Здесь принципиально НЕТ настоящей SQL-транзакции на LogStep+bump — и это
// решение по существу, не экономия скоупа. Причина первая: bump — метод
// Source, реализованный в 5 разных пакетах поверх ИХ собственных *Pool;
// общая транзакция потребовала бы менять интерфейс Source во всех них.
// Причина вторая, важнее: между LogStep и bump стоит notifyStep — постановка
// задачи во ВНЕШНИЙ outbox, побочный эффект, который транзакция БД откатить
// не может. Смерть процесса СРАЗУ ПОСЛЕ успешного notifyStep, но ДО коммита
// гипотетической tx(log+bump), дала бы ровно тот же дубль пейджа на
// следующем тике, что и в этом дизайне без транзакции — транзакция убрала
// бы только рассогласование МЕЖДУ логом и уровнем между собой, не убрала бы
// окно дубля-в-outbox. Идемпотентный LogStep (ON CONFLICT DO NOTHING,
// миграция 0085 — UNIQUE(source, incident, channel, step)) даёт ТУ ЖЕ
// гарантию (лог и уровень не расходятся), что дала бы транзакция, плюс
// констрейнт в схеме, переживающий будущий рефакторинг вызывающего кода.
//
// Итог, честно: доставка (notifyStep→outbox) НЕ транзакционна ни в каком из
// рассмотренных дизайнов, и окно "процесс упал между успешной отправкой и
// записью об этом" остаётся возможным всегда — на следующем тике получатель
// увидит повторное уведомление (дубль пейджа). Это НЕ новый техдолг этой
// правки — тот же осознанный компромисс уже стоял в докблоке чуть ниже для
// частичного провала notifyStep ("прогресс важнее стагнации") ещё до находки
// 3. Что действительно устраняет эта находка — расхождение МЕЖДУ
// escalation_level и incident_escalations между собой (дыра в логе) и
// повторный тик после краха, который раньше падал бы на UNIQUE-нарушении,
// не будь LogStep идемпотентным.
func SendStepIfDue(ctx context.Context, ladder Ladder, source string, pool *pgxpool.Pool, incidentID int64, level int, elapsed time.Duration,
	notifyStep func(channelIDs []int64, step int) ([]int64, error), bump func(id int64, from int) (bool, error)) (sent bool, err error) {
	if level >= len(ladder) {
		return false, nil
	}
	if elapsed < time.Duration(ladder[level].DelayMinutes)*time.Minute {
		return false, nil
	}
	enqueued, notifyErr := notifyStep(ladder[level].ChannelIDs, level)
	if notifyErr != nil && len(enqueued) == 0 {
		// ТОТАЛЬНЫЙ сбой: notifyStep вернул ошибку И ни один канал не
		// заенкенился — не бампим, следующий тик повторит эту же ступень
		// целиком. len(enqueued)==0 БЕЗ ошибки (напр. в лесенке нет ни одного
		// канала — проект без alert-каналов) сюда не попадает: notifyStep не
		// провалился, бампить дальше можно и нужно, как и раньше.
		return false, notifyErr
	}
	// Логируем РЕАЛЬНО заенкенные каналы ДАЖЕ при ошибке notifyStep — они уже
	// в очереди, и recovery должен про них знать (иначе пробел отбоя для тех,
	// кого реально запейджило). QA P2-3. LogStepChannels — общая точка
	// логирования+ретрая (W3-E: вынесена из этой функции, чтобы
	// uptime.Detector.notifyOpen могла ретраить лог шага 0 ТЕМ ЖЕ
	// механизмом/потолком попыток, минуя саму лесенку, не заводя вторую
	// копию).
	done, logErr := LogStepChannels(ctx, pool, source, incidentID, level, enqueued)
	if !done {
		// Находка 3: хотя бы один реально заенкененный канал не залогирован,
		// граница попыток ещё не исчерпана — не бампим, чтобы
		// escalation_level не обогнал incident_escalations. Следующий тик
		// повторит эту же ступень целиком (и лог, и notifyStep — здесь это
		// нормально: повторная отправка ступени лесенки ЕСТЬ её замысел, в
		// отличие от uptime-шага 0, см. LogStepChannels).
		return false, errors.Join(notifyErr, logErr)
	}
	// Либо notifyStep не ошибся (обычный путь, enqueued может быть и пуст —
	// каналов в лесенке просто не было), либо хотя бы один канал реально
	// получил ступень при частичном сбое, либо граница попыток логирования
	// продавила принудительный прогресс (LogStepChannels done=true с
	// logErr!=nil) — продвигаем уровень, чтобы плохой канал/лог не клинил
	// лесенку бесконечным пере-пейджем/пере-логом здоровых. Каналы, не
	// попавшие в enqueued при частичном сбое, пропустят эту ступень —
	// осознанный компромисс: прогресс важнее стагнации.
	ok, bumpErr := bump(incidentID, level)
	return ok, errors.Join(notifyErr, logErr, bumpErr)
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
