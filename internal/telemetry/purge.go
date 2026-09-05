// Package telemetry чистит телеметрию проекта/субъекта в ClickHouse.
//
// ClickHouse-каскадов нет: удаление проекта в PostgreSQL не трогает events,
// transactions, spans и прочие CH-таблицы. Purger закрывает это — по требованиям
// 152-ФЗ (удаление проекта целиком) и по праву субъекта на удаление ПДн.
package telemetry

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// projectTables — фиксированный whitelist таблиц, где данные сегментированы по
// project_id. Имена берутся ТОЛЬКО отсюда, никогда из пользовательского ввода:
// они подставляются в текст запроса напрямую (параметризовать имя таблицы нельзя).
// transactions_5m и web_vitals_5m — материализованные представления со своим
// хранилищем; ALTER ... DELETE по имени MV идёт по его внутренней таблице.
var projectTables = []string{
	"events",
	"transactions",
	"spans",
	"metric_points",
	"profile_samples",
	"check_results",
	"logs",
	"transactions_5m",
	"web_vitals_5m",
}

// Subject описывает субъекта ПДн. Достаточно одного непустого поля.
type Subject struct {
	Email  string
	UserID string
	IP     string
}

// PurgeResult — сколько строк совпало с критериями субъекта, по таблицам.
// Считается ДО удаления теми же условиями.
//
// Нужен потому, что удаление ПДн умело завершаться «успешно», не тронув ни
// одной строки, и вызывающий не мог их различить. Случай не гипотетический, а
// поведение по умолчанию: GOTCHA_SCRUB_IP и GOTCHA_SCRUB_EMAIL включены, то
// есть колонки events.user_email и events.user_ip зануляются ещё на приёме, и
// поиск субъекта по email или IP не совпадает ни с чем никогда. Работает только
// user_id — он намеренно исключён из скрубинга ровно для этого. Владелец орга,
// исполняющий требование по ст. 14 152-ФЗ, обязан видеть разницу между
// «удалено 128 записей» и «не найдено ничего».
type PurgeResult struct {
	Events       uint64
	Transactions uint64
	Spans        uint64
	MetricPoints uint64
	Logs         uint64
}

// Total — сколько всего строк отнесено к субъекту.
func (r PurgeResult) Total() uint64 {
	return r.Events + r.Transactions + r.Spans + r.MetricPoints + r.Logs
}

// Purger удаляет телеметрию из ClickHouse.
type Purger struct {
	conn driver.Conn
}

// NewPurger создаёт Purger поверх соединения с ClickHouse.
func NewPurger(conn driver.Conn) *Purger {
	return &Purger{conn: conn}
}

// PurgeProject удаляет всю телеметрию проекта из всех таблиц whitelist'а.
// ALTER ... DELETE — мутация, асинхронная по умолчанию; mutations_sync = 2
// делает её синхронной, чтобы удаление было завершённым по возврату.
// max_execution_time = 0 снимает пер-соединенческий потолок (60с в
// clickhouse.go): синхронная мутация по 90-дневным данным идёт минуты, и без
// снятия потолка субъектное удаление (152-ФЗ) падало бы с TIMEOUT посередине,
// оставляя данные наполовину удалёнными.
func (p *Purger) PurgeProject(ctx context.Context, projectID int64) error {
	for _, t := range projectTables {
		q := "ALTER TABLE " + t + " DELETE WHERE project_id = ? SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, q, projectID); err != nil {
			return fmt.Errorf("telemetry: purge project %d from %s: %w", projectID, t, err)
		}
	}
	return nil
}

// PurgeSubject удаляет ПДн субъекта в рамках проекта. Надёжно матчатся и
// удаляются строки, где субъект выделяется по ТОЧНОМУ значению поля:
//   - events: колонки user_email / user_id / user_ip;
//   - transactions: колонка user_id и теги tags['user.id']/tags['enduser.id']
//     (← UserID), tags['user.email']/tags['enduser.email'] (← Email) — OTLP-приём
//     кладёт атрибуты спана в tags как есть, поэтому субъект по email виден в
//     transactions только через теги (см. txSubjectConds);
//   - spans: у таблицы spans (ch/0004_spans.up.sql) вообще нет колонки субъекта —
//     ни user_id, ни тегов. Субъект адресуется КОСВЕННО, через trace_id его
//     транзакций: сначала собираются trace_id строк transactions, совпавших с
//     субъектом по тем же условиям, что и сама очистка transactions
//     (txSubjectConds), и уже по этому списку удаляются spans с тем же
//     project_id и trace_id IN (...). Удаление spans идёт ДО удаления
//     transactions (а не после, хотя оба списка формируются из одних и тех же
//     trace_id) — ради повторяемости при сбое: если операция не atomic (а она
//     не atomic — это два независимых ALTER ... DELETE) и упадёт на какой-то
//     пачке spans, transactions ещё целы, и повторный вызов PurgeSubject
//     соберёт те же trace_id заново и продолжит. Переставь порядок обратно —
//     и сбой на spans после того, как transactions уже стёрты, станет
//     неисправим: retry не найдёт trace_id субъекта, а недоудалённые spans
//     станут неотличимы от законно осиротевших;
//   - metric_points: attributes['user.id']/['enduser.id'] (← UserID),
//     attributes['user.email'] (← Email);
//   - logs: log_attributes['user.id']/['enduser.id'] (← UserID),
//     log_attributes['user.email']/['enduser.email'] (← Email).
//
// НЕ чистятся программно free-form поля, где субъекта нельзя выделить надёжно, не
// рискуя удалить чужое или пропустить нужное: spans.data и spans.description
// (произвольный JSON/URL/SQL от SDK — субъект в них не адресуется по ключу),
// profile_samples.stack (кадры стека; ПДн там практически не бывает), а также
// logs.body (произвольный текст сообщения приложения — то же самое соображение).
// Эти поля обезличиваются ретенцией по TTL из миграций ch/: spans — 30 дней,
// transactions — 90 дней, metric_points — 30 дней, profile_samples — 7 дней,
// logs — 14 дней. Решение владельца: profile_samples.stack программно не
// чистится и остаётся на TTL 7 дней даже после удаления субъекта — то же
// соображение о free-form поле, что и для spans.data/description.
//
// Остаток, который переживает вызов (границы механизма, а не забытые случаи):
//   - спаны трейса, у которого нет строки в transactions (например, транзакция
//     уже вытеснена ретенцией, а спаны — ещё нет), не находятся по trace_id и
//     доживают до собственного TTL (30 дней);
//   - трейс, в котором участвовали несколько субъектов (фоновые задания,
//     межпользовательские запросы), удаляется ЦЕЛИКОМ, если хотя бы одна его
//     транзакция принадлежит субъекту — spans не делится по субъектам внутри
//     трейса, и это считается приемлемым.
//
// В events, transactions, metric_points и logs удаляются строки, совпавшие ХОТЯ
// БЫ по одному непустому критерию субъекта. Пустые поля Subject в условие не
// попадают.
func (p *Purger) PurgeSubject(ctx context.Context, projectID int64, sub Subject) (PurgeResult, error) {
	var res PurgeResult

	// events: OR по всем непустым идентификаторам субъекта.
	var conds []string
	var args []any
	args = append(args, projectID)
	if sub.Email != "" {
		conds = append(conds, "user_email = ?")
		args = append(args, sub.Email)
	}
	if sub.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, sub.UserID)
	}
	if sub.IP != "" {
		conds = append(conds, "user_ip = ?")
		args = append(args, sub.IP)
	}
	if len(conds) == 0 {
		return res, fmt.Errorf("telemetry: purge subject: empty subject")
	}

	where := "project_id = ? AND (" + strings.Join(conds, " OR ") + ")"
	n, err := p.countMatching(ctx, "events", where, args)
	if err != nil {
		return res, err
	}
	res.Events = n
	eventsQ := "ALTER TABLE events DELETE WHERE " + where +
		" SETTINGS mutations_sync = 2, max_execution_time = 0"
	if err := p.conn.Exec(ctx, eventsQ, args...); err != nil {
		return res, fmt.Errorf("telemetry: purge subject from events (project %d): %w", projectID, err)
	}

	// transactions: субъект живёт в колонке user_id и в тегах (см. txSubjectConds).
	// Матчим по обоим, иначе субъект, заданный email, не удаляет свои транзакции.
	//
	// ПОРЯДОК ОБЯЗАТЕЛЕН, и он не «собрать → удалить транзакции → удалить
	// spans»: spans удаляются РАНЬШЕ transactions. trace_id собираются один
	// раз в начале и не меняются, но если удалить транзакции первыми, а потом
	// упадёт удаление spans (несколько независимых ALTER ... DELETE, не
	// atomic), повторный вызов PurgeSubject уже не найдёт trace_id субъекта —
	// транзакций больше нет — и недоудалённые spans останутся в базе
	// неотличимыми от законно осиротевших. Удаляя spans первыми, сбой на них
	// оставляет transactions нетронутыми, и retry просто повторяет всю
	// операцию заново.
	if txConds, txArgs := txSubjectConds(sub); len(txConds) > 0 {
		args := append([]any{projectID}, txArgs...)
		txWhere := "project_id = ? AND (" + strings.Join(txConds, " OR ") + ")"

		traceIDs, err := p.matchingTraceIDs(ctx, txWhere, args)
		if err != nil {
			return res, err
		}

		// spans: субъект в этой таблице не адресуется напрямую (нет ни колонки,
		// ни тегов) — только косвенно, через trace_id, собранный строкой выше,
		// пока транзакции ещё на месте. Удаляются ДО transactions — см.
		// комментарий выше про идемпотентность retry.
		spansDeleted, err := p.purgeSpansByTraceIDs(ctx, projectID, traceIDs)
		if err != nil {
			return res, err
		}
		res.Spans = spansDeleted

		n, err := p.countMatching(ctx, "transactions", txWhere, args)
		if err != nil {
			return res, err
		}
		res.Transactions = n
		txQ := "ALTER TABLE transactions DELETE WHERE " + txWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, txQ, args...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from transactions (project %d): %w", projectID, err)
		}
	}

	// metric_points несут ПДн в attributes (Map(String,String)): OTel-конвенции
	// кладут туда user.id/enduser.id/user.email. Чистим по непустым полям
	// субъекта. user_ip в attributes не встречается, поэтому в условие не входит.
	var mpConds []string
	mpArgs := []any{projectID}
	if sub.UserID != "" {
		mpConds = append(mpConds, "attributes['user.id'] = ?", "attributes['enduser.id'] = ?")
		mpArgs = append(mpArgs, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		mpConds = append(mpConds, "attributes['user.email'] = ?")
		mpArgs = append(mpArgs, sub.Email)
	}
	if len(mpConds) > 0 {
		mpWhere := "project_id = ? AND (" + strings.Join(mpConds, " OR ") + ")"
		n, err := p.countMatching(ctx, "metric_points", mpWhere, mpArgs)
		if err != nil {
			return res, err
		}
		res.MetricPoints = n
		mpQ := "ALTER TABLE metric_points DELETE WHERE " + mpWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, mpQ, mpArgs...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from metric_points (project %d): %w", projectID, err)
		}
	}

	// logs несут ПДн субъекта в log_attributes (Map(String,String)): OTel-конвенции
	// кладут туда user.id/enduser.id/user.email/enduser.email. Чистим по непустым
	// полям субъекта. user_ip в log_attributes не встречается, поэтому в условие
	// не входит (как и в metric_points). body — free-form, программно не чистится,
	// обезличивается TTL (14 дней, см. ch/0020_logs.up.sql).
	var logConds []string
	logArgs := []any{projectID}
	if sub.UserID != "" {
		logConds = append(logConds, "log_attributes['user.id'] = ?", "log_attributes['enduser.id'] = ?")
		logArgs = append(logArgs, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		logConds = append(logConds, "log_attributes['user.email'] = ?", "log_attributes['enduser.email'] = ?")
		logArgs = append(logArgs, sub.Email, sub.Email)
	}
	if len(logConds) > 0 {
		logWhere := "project_id = ? AND (" + strings.Join(logConds, " OR ") + ")"
		n, err := p.countMatching(ctx, "logs", logWhere, logArgs)
		if err != nil {
			return res, err
		}
		res.Logs = n
		logQ := "ALTER TABLE logs DELETE WHERE " + logWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, logQ, logArgs...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from logs (project %d): %w", projectID, err)
		}
	}
	return res, nil
}

// countMatching считает строки, попадающие под условие удаления. Имя таблицы
// приходит только из литералов этого файла (параметризовать его нельзя), where
// собран из фиксированных фрагментов, все значения — связанные параметры.
func (p *Purger) countMatching(ctx context.Context, table, where string, args []any) (uint64, error) {
	var n uint64
	q := "SELECT count() FROM " + table + " WHERE " + where
	if err := p.conn.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("telemetry: count subject rows in %s: %w", table, err)
	}
	return n, nil
}

// matchingTraceIDs возвращает различные trace_id транзакций, совпавших с
// условием where (то же условие, что использует само удаление transactions).
// Вызывается ДО удаления этих строк — потом trace_id субъекта взять уже
// неоткуда (см. PurgeSubject).
func (p *Purger) matchingTraceIDs(ctx context.Context, where string, args []any) ([]string, error) {
	rows, err := p.conn.Query(ctx, "SELECT DISTINCT trace_id FROM transactions WHERE "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: collect subject trace_id: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("telemetry: scan subject trace_id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry: collect subject trace_id: %w", err)
	}
	return ids, nil
}

// spanTraceIDBatch — сколько trace_id попадает в один запрос удаления spans.
// Субъект теоретически мог оставить очень много транзакций за окно хранения
// (90 дней), и передавать их все одним bind-параметром неограниченного размера
// рискованно — как по объёму самого запроса, так и по памяти на стороне
// ClickHouse при его разборе. Резать на пачки этого размера дороже по числу
// ALTER-мутаций, но каждая остаётся дешёвой и предсказуемой; порог взят с
// большим запасом над реалистичным числом трейсов одного субъекта.
const spanTraceIDBatch = 5000

// purgeSpansByTraceIDs считает и удаляет строки spans проекта, чьи trace_id
// входят в переданный список. Список приходит из matchingTraceIDs и режется на
// пачки spanTraceIDBatch, чтобы не собирать неограниченно большой IN(...).
func (p *Purger) purgeSpansByTraceIDs(ctx context.Context, projectID int64, traceIDs []string) (uint64, error) {
	var total uint64
	for len(traceIDs) > 0 {
		batch := traceIDs
		if len(batch) > spanTraceIDBatch {
			batch = traceIDs[:spanTraceIDBatch]
		}
		traceIDs = traceIDs[len(batch):]

		const where = "project_id = ? AND trace_id IN (?)"
		args := []any{projectID, batch}
		n, err := p.countMatching(ctx, "spans", where, args)
		if err != nil {
			return total, err
		}
		total += n

		q := "ALTER TABLE spans DELETE WHERE " + where +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, q, args...); err != nil {
			return total, fmt.Errorf("telemetry: purge subject spans (project %d): %w", projectID, err)
		}
	}
	return total, nil
}

// txSubjectConds строит OR-условия и bound-параметры, относящие строку
// transactions к субъекту. transactions хранят субъекта двумя способами:
//   - колонка user_id — заполняется Sentry-приёмом (contexts.user.id);
//   - теги tags (Map(String,String)) — OTLP-приём кладёт туда атрибуты спана как
//     есть, включая OTel-идентификаторы субъекта. По ним матчим ТОЧНО, по ключу:
//     user.id/enduser.id ← UserID, user.email/enduser.email ← Email.
//
// Совпадение ХОТЯ БЫ по одному критерию относит строку к субъекту. IP в
// transactions не хранится (нет колонки, в теги приём его не кладёт), поэтому
// IP-only субъект не даёт условий и транзакции не затрагивает — как и раньше.
// Порядок conds/args согласован: N-е условие связано с N-м параметром.
func txSubjectConds(sub Subject) (conds []string, args []any) {
	if sub.UserID != "" {
		conds = append(conds, "user_id = ?", "tags['user.id'] = ?", "tags['enduser.id'] = ?")
		args = append(args, sub.UserID, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		conds = append(conds, "tags['user.email'] = ?", "tags['enduser.email'] = ?")
		args = append(args, sub.Email, sub.Email)
	}
	return conds, args
}
