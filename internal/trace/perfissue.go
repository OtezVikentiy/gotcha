package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound      = errors.New("trace: perf issue not found")
	ErrInvalidStatus = errors.New("trace: invalid perf issue status")
)

// validStatuses — единственные допустимые значения perf_issues.status
// (совпадают с CHECK-constraint миграции 0007).
var validStatuses = map[string]bool{
	"unresolved": true,
	"resolved":   true,
	"ignored":    true,
}

// Лимиты выборки List: без верхней границы страница «отдай всё» вытянула бы
// в память все проблемы проекта.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// perfSampleTTL — как часто повторное обнаружение освежает пример проблемы
// (evidence + sample_trace_id). Час — компромисс между «пример не должен
// протухнуть вместе с трейсом в CH» и «горячую строку нельзя переписывать на
// каждой семплированной транзакции» (см. Record).
const perfSampleTTL = time.Hour

// PerfIssue — строка perf_issues: найденная детекторами проблема
// производительности, сгруппированная по (project_id, fingerprint). Живёт в PG
// (а не в CH, где лежат спаны), потому что у неё есть состояние: статус,
// счётчик, first/last seen.
type PerfIssue struct {
	ID        int64
	ProjectID int64

	Fingerprint string
	Kind        string // KindNPlusOne | KindSlowDBQuery | KindHTTPFlood
	Title       string
	Culprit     string
	Status      string // unresolved | resolved | ignored

	Count     int64
	FirstSeen time.Time
	LastSeen  time.Time

	SampleTraceID string          // трейс последнего обнаружения — с него открывают waterfall
	Evidence      json.RawMessage // count/total_ms/span_ids и поля, специфичные для вида
}

// IssueService — perf_issues в PostgreSQL: upsert находок и их жизненный цикл.
type IssueService struct {
	pool *pgxpool.Pool
}

func NewIssueService(pool *pgxpool.Pool) *IssueService {
	return &IssueService{pool: pool}
}

const perfIssueColumns = `id, project_id, fingerprint, kind, title, culprit, status,
	count, first_seen, last_seen, sample_trace_id, evidence`

func scanPerfIssue(row interface{ Scan(dest ...any) error }, i *PerfIssue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Kind, &i.Title, &i.Culprit, &i.Status,
		&i.Count, &i.FirstSeen, &i.LastSeen, &i.SampleTraceID, &i.Evidence)
}

// RecordResult — что произошло с проблемой при обнаружении. Формой повторяет
// issue.UpsertResult (ошибки этапа 1): по Created шлётся алерт о первом
// обнаружении, по Regression — о вернувшейся проблеме.
type RecordResult struct {
	Issue      PerfIssue
	Created    bool // строка ФИЗИЧЕСКИ вставлена этим вызовом
	Regression bool // проблема была resolved и снова обнаружена
	Suppressed bool // строки нет: часовой кап на СОЗДАНИЕ проблем выбран (Issue пустой)
}

// MaxNewPerfIssuesPerHour — сколько НОВЫХ (project_id, fingerprint) разрешено
// создать проекту за час. Экспортирован: на него смотрят тесты пакета.
const MaxNewPerfIssuesPerHour = 100

// perfIssueWindow — окно капа на создание, «прыгающее» (tumbling), как у
// троттлинга алертов (см. perfAlertWindow).
const perfIssueWindow = time.Hour

// Record регистрирует находку детектора. Новый (project_id, fingerprint)
// создаёт проблему с count=1 и возвращает Created=true — по этому признаку
// шлётся алерт (см. notify.go): повторное обнаружение той же проблемы не должно
// будить дежурного на каждый запрос.
//
// Повтор инкрементит count и двигает last_seen. Пример (evidence + sample_trace_id)
// при этом НЕ переписывается на каждом повторе: проблема воспроизводится на
// каждом запросе к эндпойнту, и горячая строка иначе получала бы перезапись
// jsonb на каждой семплированной транзакции — лишний WAL, лишний TOAST и более
// длинная блокировка строки, на которой сериализуются воркеры пайплайна. Пример
// освежается не чаще раза в perfSampleTTL: и это не «первый навсегда» — трейсы в
// CH живут с TTL, и самый первый пример однажды исчез бы, оставив кнопку
// «открыть waterfall» ведущей в никуда. evidence и sample_trace_id обновляются
// ВМЕСТЕ: span_ids из evidence принадлежат именно тому трейсу.
//
// Проблема со статусом resolved при новом обнаружении возвращается в unresolved
// и помечается Regression=true — ровно как issues этапа 1 (см.
// issue.Service.Upsert): починенная и снова сломавшаяся проблема должна
// разбудить дежурного, а не тихо переоткрыться. ignored остаётся ignored
// (заглушили осознанно) и регрессией не считается — и last_seen у неё НЕ
// двигается: List сортирует по last_seen DESC, и заглушённая проблема, которую
// продолжают находить на каждом запросе, всплывала бы в самый верх списка,
// вытесняя те, на которые смотреть как раз надо. Счётчик при этом растёт: когда
// проблему разглушат, история не потеряется.
//
// Всё это делает один атомарный upsert (ON CONFLICT ... DO UPDATE), а не
// select-then-update: параллельные воркеры пайплайна пишут одну и ту же
// проблему без гонки.
//
// Created берётся из `xmax = 0` — то есть из того, что физически произошло с
// СТРОКОЙ, а не из снимка. Снимок здесь врёт: CTE old читает состояние на начало
// запроса, а ON CONFLICT DO UPDATE перечитывает конфликтующую строку ВНЕ его
// (EvalPlanQual), поэтому при гонке двух первых обнаружений проигравший видел бы
// old.status IS NULL и тоже возвращал created=true — алерт о первом обнаружении
// ушёл бы дважды. Троттлинг рассылки (OutboxNotifier.claimAlert) такую пару
// съел бы как два слота из часового лимита проекта, а не как один — то есть
// гонку он маскирует, но не закрывает; закрывает её именно xmax.
//
// Regression по-прежнему считается по снимку: старого статуса в RETURNING не
// достать. Гонка здесь безобиднее (нужны два одновременных обнаружения именно
// РЕЗОЛВНУТОЙ проблемы) и в худшем случае даёт второй алерт о регрессии.
//
// СОЗДАНИЕ новых проблем ограничено часовым капом проекта (см. claimNewIssue):
// сначала пробуем обновить существующую строку (обычный случай — один запрос),
// и только если её нет, занимаем слот. Кап выбран → строки НЕ будет, вернётся
// RecordResult{Suppressed: true}, а подавление уедет в лог. Повторные
// обнаружения уже созданных проблем кап не трогает.
func (s *IssueService) Record(ctx context.Context, projectID int64, f Finding, traceID string) (RecordResult, error) {
	evidence, err := json.Marshal(f.Evidence)
	if err != nil || f.Evidence == nil {
		// Нечитаемое evidence не повод терять находку: пишем пустой объект.
		evidence = []byte(`{}`)
	}

	// Граница устаревания примера: строки, чей пример старше, обновляют
	// evidence/sample_trace_id, остальные оставляют их как есть.
	sampleCutoff := time.Now().Add(-perfSampleTTL)

	res, found, err := s.touch(ctx, projectID, f, evidence, traceID, sampleCutoff)
	if err != nil {
		return RecordResult{}, err
	}
	if found {
		return res, nil
	}

	claimed, suppressed, err := s.claimNewIssue(ctx, projectID)
	if err != nil {
		return RecordResult{}, err
	}
	if !claimed {
		// Молчаливый кап читался бы как «мы нашли всё»: сколько находок осталось
		// без строки — в лог.
		slog.Warn("perf issue creation capped, finding recorded nowhere",
			"project_id", projectID, "kind", f.Kind, "culprit", f.Culprit,
			"fingerprint", f.Fingerprint, "limit_per_hour", MaxNewPerfIssuesPerHour,
			"suppressed_in_window", suppressed)
		return RecordResult{Suppressed: true}, nil
	}
	return s.create(ctx, projectID, f, evidence, traceID)
}

// touch инкрементит СУЩЕСТВУЮЩУЮ проблему. found=false — строки нет (её создание
// проходит через кап, см. Record). Логика обновления та же, что была в
// upsert-е: count, last_seen (кроме ignored), освежение примера не чаще
// perfSampleTTL, resolved → unresolved с признаком регрессии.
func (s *IssueService) touch(ctx context.Context, projectID int64, f Finding,
	evidence []byte, traceID string, sampleCutoff time.Time) (RecordResult, bool, error) {

	const q = `
WITH old AS (
    SELECT status FROM perf_issues WHERE project_id = $1 AND fingerprint = $2
), up AS (
    UPDATE perf_issues SET
        kind            = $3,
        title           = $4,
        culprit         = $5,
        count           = perf_issues.count + 1,
        last_seen       = CASE WHEN status = 'ignored' THEN last_seen ELSE now() END,
        sample_trace_id = CASE WHEN sample_at <= $8 THEN $6 ELSE sample_trace_id END,
        evidence        = CASE WHEN sample_at <= $8 THEN $7::jsonb ELSE evidence END,
        sample_at       = CASE WHEN sample_at <= $8 THEN now() ELSE sample_at END,
        status          = CASE WHEN status = 'resolved' THEN 'unresolved' ELSE status END
    WHERE project_id = $1 AND fingerprint = $2
    RETURNING ` + perfIssueColumns + `
)
SELECT up.id, up.project_id, up.fingerprint, up.kind, up.title, up.culprit, up.status,
       up.count, up.first_seen, up.last_seen, up.sample_trace_id, up.evidence,
       coalesce(old.status = 'resolved', false) AS regression
FROM up LEFT JOIN old ON true`

	var r RecordResult
	row := s.pool.QueryRow(ctx, q,
		projectID, f.Fingerprint, f.Kind, f.Title, f.Culprit, traceID, evidence, sampleCutoff)
	if err := row.Scan(&r.Issue.ID, &r.Issue.ProjectID, &r.Issue.Fingerprint, &r.Issue.Kind, &r.Issue.Title,
		&r.Issue.Culprit, &r.Issue.Status, &r.Issue.Count, &r.Issue.FirstSeen, &r.Issue.LastSeen,
		&r.Issue.SampleTraceID, &r.Issue.Evidence, &r.Regression); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecordResult{}, false, nil // проблемы ещё нет
		}
		return RecordResult{}, false, fmt.Errorf("trace: record perf issue: %w", err)
	}
	return r, true, nil
}

// create вставляет новую проблему. Слот часового капа уже занят (см. Record).
// Остаётся ON CONFLICT: между touch и create ту же проблему мог создать соседний
// воркер — тогда это обычный инкремент, created=false (слот при этом потрачен
// впустую, но такая гонка — событие на пару миллисекунд в час, а не режим работы).
func (s *IssueService) create(ctx context.Context, projectID int64, f Finding,
	evidence []byte, traceID string) (RecordResult, error) {

	const q = `
WITH old AS (
    SELECT status FROM perf_issues WHERE project_id = $1 AND fingerprint = $2
), up AS (
    INSERT INTO perf_issues (project_id, fingerprint, kind, title, culprit, count, sample_trace_id, evidence)
    VALUES ($1, $2, $3, $4, $5, 1, $6, $7)
    ON CONFLICT (project_id, fingerprint) DO UPDATE SET
        kind      = EXCLUDED.kind,
        title     = EXCLUDED.title,
        culprit   = EXCLUDED.culprit,
        count     = perf_issues.count + 1,
        last_seen = CASE WHEN perf_issues.status = 'ignored'
                         THEN perf_issues.last_seen ELSE now() END,
        status    = CASE WHEN perf_issues.status = 'resolved' THEN 'unresolved' ELSE perf_issues.status END
    RETURNING ` + perfIssueColumns + `, (xmax = 0) AS created
)
SELECT up.id, up.project_id, up.fingerprint, up.kind, up.title, up.culprit, up.status,
       up.count, up.first_seen, up.last_seen, up.sample_trace_id, up.evidence,
       up.created,
       (NOT up.created AND coalesce(old.status = 'resolved', false)) AS regression
FROM up LEFT JOIN old ON true`

	var r RecordResult
	row := s.pool.QueryRow(ctx, q,
		projectID, f.Fingerprint, f.Kind, f.Title, f.Culprit, traceID, evidence)
	if err := row.Scan(&r.Issue.ID, &r.Issue.ProjectID, &r.Issue.Fingerprint, &r.Issue.Kind, &r.Issue.Title,
		&r.Issue.Culprit, &r.Issue.Status, &r.Issue.Count, &r.Issue.FirstSeen, &r.Issue.LastSeen,
		&r.Issue.SampleTraceID, &r.Issue.Evidence, &r.Created, &r.Regression); err != nil {
		return RecordResult{}, fmt.Errorf("trace: record perf issue: %w", err)
	}
	return r, nil
}

// claimNewIssue атомарно занимает один слот из MaxNewPerfIssuesPerHour на
// СОЗДАНИЕ проблемы. Устроен как claimAlert (см. notify.go): проверка «влезаем
// ли в окно» и отметка «занято» — один statement с ON CONFLICT, чтобы
// параллельные воркеры пайплайна сериализовались на блокировке строки, а не
// гонялись ровно там, где рост таблицы и надо остановить.
//
// claimed=false — лимит окна выбран, строку создавать нельзя. Тогда счётчик
// suppressed окна инкрементится отдельным UPDATE (в лог уходит, сколько находок
// уже осталось без строки), и его значение возвращается. suppressed растёт без
// капа: это просто счётчик подавленного за окно, на решение он не влияет.
func (s *IssueService) claimNewIssue(ctx context.Context, projectID int64) (claimed bool, suppressed int, err error) {
	cutoff := time.Now().Add(-perfIssueWindow)
	var created int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO perf_issue_throttle (project_id, window_start, created, suppressed)
		VALUES ($1, now(), 1, 0)
		ON CONFLICT (project_id) DO UPDATE SET
			window_start = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN now() ELSE perf_issue_throttle.window_start END,
			created      = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN 1 ELSE perf_issue_throttle.created + 1 END,
			suppressed   = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN 0 ELSE perf_issue_throttle.suppressed END
		WHERE perf_issue_throttle.window_start <= $2 OR perf_issue_throttle.created < $3
		RETURNING created`,
		projectID, cutoff, MaxNewPerfIssuesPerHour).Scan(&created)
	if err == nil {
		return true, 0, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, fmt.Errorf("trace: claim new perf issue: %w", err)
	}
	// Лимит окна выбран: отмечаем подавление и возвращаем его счётчик для лога.
	if err := s.pool.QueryRow(ctx, `
		UPDATE perf_issue_throttle SET suppressed = suppressed + 1
		WHERE project_id = $1 RETURNING suppressed`, projectID).Scan(&suppressed); err != nil {
		return false, 0, fmt.Errorf("trace: mark perf issue suppressed: %w", err)
	}
	return false, suppressed, nil
}

// List возвращает проблемы проекта, свежие первыми. Пустой status — все
// статусы; невалидный status → ErrInvalidStatus (иначе фильтр молча вернул бы
// пустой список).
func (s *IssueService) List(ctx context.Context, projectID int64, status string, limit int) ([]PerfIssue, error) {
	if status != "" && !validStatuses[status] {
		return nil, ErrInvalidStatus
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// $2 = '' означает «без фильтра по статусу» — одна форма запроса вместо
	// склейки SQL-строки.
	rows, err := s.pool.Query(ctx, `SELECT `+perfIssueColumns+`
		FROM perf_issues
		WHERE project_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY last_seen DESC
		LIMIT $3`, projectID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("trace: list perf issues: %w", err)
	}
	defer rows.Close()

	var out []PerfIssue
	for rows.Next() {
		var i PerfIssue
		if err := scanPerfIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("trace: list perf issues scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: list perf issues: %w", err)
	}
	return out, nil
}

// Get возвращает проблему проекта по id или ErrNotFound. projectID —
// обязательная часть условия, а не украшение: id проблемы глобален, и выборка
// «по одному id» на будущем маршруте /perf-issues/{id} отдавала бы чужую
// проблему любому, кто угадал число (IDOR). Чужая проблема неотличима от
// несуществующей — ErrNotFound.
func (s *IssueService) Get(ctx context.Context, projectID, id int64) (PerfIssue, error) {
	var i PerfIssue
	row := s.pool.QueryRow(ctx,
		"SELECT "+perfIssueColumns+" FROM perf_issues WHERE project_id = $1 AND id = $2", projectID, id)
	if err := scanPerfIssue(row, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerfIssue{}, ErrNotFound
		}
		return PerfIssue{}, fmt.Errorf("trace: get perf issue: %w", err)
	}
	return i, nil
}

// ProjectOf резолвит владеющий проблемой проект по её глобальному id.
// Маршрут /perf-issues/{id} несёт в пути только id, а Get/SetStatus скоуплены
// по проекту (см. их комментарии про IDOR) — поэтому сперва узнаём, чей это
// проект, затем проверяем на него доступ, и только потом читаем строку уже
// скоуплено. found=false — проблемы нет; снаружи чужая и несуществующая
// проблема неотличимы (обе дают 404, без оракула существования чужих id).
func (s *IssueService) ProjectOf(ctx context.Context, id int64) (projectID int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, "SELECT project_id FROM perf_issues WHERE id = $1", id).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("trace: perf issue project: %w", err)
	}
	return projectID, true, nil
}

// SetStatus меняет статус проблемы: unresolved | resolved | ignored.
// Невалидный статус → ErrInvalidStatus, отсутствующая проблема → ErrNotFound.
//
// projectID — обязательная часть условия ровно по той же причине, что и в Get:
// id проблемы глобален, и запрос без него дал бы участнику одной организации
// возможность закрыть или заглушить проблему ЧУЖОГО проекта по угаданному числу
// (IDOR на будущем маршруте POST /perf-issues/{id}/status). Чужая проблема
// неотличима от несуществующей — ErrNotFound.
func (s *IssueService) SetStatus(ctx context.Context, projectID, id int64, status string) error {
	if !validStatuses[status] {
		return ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx,
		"UPDATE perf_issues SET status = $1 WHERE project_id = $2 AND id = $3", status, projectID, id)
	if err != nil {
		return fmt.Errorf("trace: set perf issue status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
