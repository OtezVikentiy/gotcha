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
	MetricPoints uint64
}

// Total — сколько всего строк отнесено к субъекту.
func (r PurgeResult) Total() uint64 {
	return r.Events + r.Transactions + r.MetricPoints
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
//   - metric_points: attributes['user.id']/['enduser.id'] (← UserID),
//     attributes['user.email'] (← Email).
//
// НЕ чистятся программно free-form поля, где субъекта нельзя выделить надёжно, не
// рискуя удалить чужое или пропустить нужное: spans.data и spans.description
// (произвольный JSON/URL/SQL от SDK — субъект в них не адресуется по ключу),
// а также profile_samples.stack (кадры стека; ПДн там практически не бывает).
// Эти поля обезличиваются ретенцией по TTL из миграций ch/: spans — 30 дней,
// transactions — 90 дней, metric_points — 30 дней, profile_samples — 7 дней.
//
// В events, transactions и metric_points удаляются строки, совпавшие ХОТЯ БЫ по
// одному непустому критерию субъекта. Пустые поля Subject в условие не попадают.
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
	if txConds, txArgs := txSubjectConds(sub); len(txConds) > 0 {
		args := append([]any{projectID}, txArgs...)
		txWhere := "project_id = ? AND (" + strings.Join(txConds, " OR ") + ")"
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
