package issue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNotFound      = errors.New("issue: not found")
	ErrInvalidStatus = errors.New("issue: invalid status")
)

// Статусы issue. Как и уровни ниже (Levels), раньше набор был размножен
// литералами по внешним потребителям — internal/web/issues.go (дефолт
// фильтра списка и bulkActionStatus) и internal/web/exports.go
// (exportValidStatuses, копия из докблока «issue не экспортирует их
// набором») — ни одна из копий не сверялась друг с другом. Экспортированные
// константы и Statuses делают issue единственным владельцем набора: обе
// копии переведены на них.
const (
	StatusUnresolved = "unresolved"
	StatusResolved   = "resolved"
	StatusIgnored    = "ignored"
)

// Statuses — все допустимые статусы issues.status (совпадают с
// CHECK-constraint в миграции), в порядке, использующемся в дропдауне
// фильтра/форме выгрузки.
var Statuses = []string{StatusUnresolved, StatusResolved, StatusIgnored}

var validStatuses = func() map[string]bool {
	m := make(map[string]bool, len(Statuses))
	for _, s := range Statuses {
		m[s] = true
	}
	return m
}()

// IsValidStatus сообщает, входит ли v в Statuses. Пустая строка — не
// статус (фильтры трактуют "" отдельно, как «без ограничения»,
// см. buildIssueFilter/Filter) — IsValidStatus для неё возвращает false.
func IsValidStatus(v string) bool { return validStatuses[v] }

// Уровни issue. В отличие от status, колонка issues.level не ограничена
// CHECK в схеме (см. миграцию 0003) — единственный источник истины для
// множества значений раньше был разбросан: приём (internal/ingest) держал
// свою копию списка для валидации, веб-слой — свою для рендера бейджа и
// дропдауна фильтра. Экспортированные константы делают issue владельцем
// набора: приём и веб теперь ссылаются на них вместо повторения строк.
const (
	LevelDebug   = "debug"
	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
	LevelFatal   = "fatal"
)

// Levels — все допустимые уровни issue, по возрастанию серьёзности. Порядок
// важен: это же он используется в дропдауне фильтра.
var Levels = []string{LevelDebug, LevelInfo, LevelWarning, LevelError, LevelFatal}

var validLevels = func() map[string]bool {
	m := make(map[string]bool, len(Levels))
	for _, l := range Levels {
		m[l] = true
	}
	return m
}()

// IsValidLevel сообщает, входит ли v в Levels. Пустая строка — не уровень
// (фильтры/форма выгрузки трактуют "" отдельно, как «без ограничения») —
// IsValidLevel для неё возвращает false.
func IsValidLevel(v string) bool { return validLevels[v] }

// sortColumns — whitelist сортировки: в SQL-текст попадает только
// это заранее заданное выражение, никогда пользовательская строка.
var sortColumns = map[string]string{
	"last_seen":  "issues.last_seen DESC",
	"first_seen": "issues.first_seen DESC",
	"times_seen": "issues.times_seen DESC",
}

const defaultSort = "last_seen"

const (
	defaultPerPage = 25
	maxPerPage     = 100
)

// Filter — параметры выборки issue в List.
type Filter struct {
	Status      string // "", unresolved, resolved, ignored
	Level       string // "", debug..fatal
	Query       string // подстрока в title/culprit (ILIKE)
	Sort        string // last_seen (default) | first_seen | times_seen
	Environment string // "" = все окружения; иначе EXISTS по issue_environments
	// Since/Until — границы окна по last_seen; нулевое значение = без границы.
	//
	// Раньше здесь была строка периода из белого списка (24h|7d|30d), и список
	// проблем поэтому имел свой, более бедный фильтр времени: ни часа, ни
	// произвольного диапазона — при том что на соседних страницах общий контрол
	// умел и то, и другое. Границы приходят параметрами запроса, поэтому любое
	// окно, которое умеет разобрать веб-слой, работает и здесь.
	Since   time.Time
	Until   time.Time
	Page    int
	PerPage int
}

const issueColumns = `id, project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen, assignee_id`

// issueColumnsJoined/issueFromJoined — то же самое, но с квалификацией
// issues. и колонкой assignee_email из LEFT JOIN users (для List/Get,
// которым нужна колонка Assignee). issueColumns (без join) остаётся для
// ActiveSince, которому assignee_email не нужен.
const issueColumnsJoined = `issues.id, issues.project_id, issues.fingerprint, issues.title, issues.culprit, issues.level, issues.status, issues.first_seen, issues.last_seen, issues.times_seen, issues.assignee_id, coalesce(u.email, '') AS assignee_email`
const issueFromJoined = `issues LEFT JOIN users u ON u.id = issues.assignee_id`

func scanIssue(row interface{ Scan(dest ...any) error }, i *Issue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
		&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID)
}

func scanIssueWithAssignee(row interface{ Scan(dest ...any) error }, i *Issue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
		&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID, &i.AssigneeEmail)
}

// buildIssueFilter собирает WHERE-условие и позиционные аргументы фильтра —
// общую часть для запроса строк и отдельного запроса total (см. List): один
// и тот же набор предикатов должен ограничивать оба запроса одинаково,
// иначе total и фактически показанный список могли бы разойтись.
func buildIssueFilter(projectID int64, f Filter) (string, []any) {
	var sb strings.Builder
	sb.WriteString("issues.project_id = $1")
	args := []any{projectID}

	if f.Status != "" {
		args = append(args, f.Status)
		fmt.Fprintf(&sb, " AND issues.status = $%d", len(args))
	}
	if f.Level != "" {
		args = append(args, f.Level)
		fmt.Fprintf(&sb, " AND issues.level = $%d", len(args))
	}
	if f.Query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query)
		args = append(args, "%"+escaped+"%")
		idx := len(args)
		fmt.Fprintf(&sb, " AND (issues.title ILIKE $%d OR issues.culprit ILIKE $%d)", idx, idx)
	}
	if f.Environment != "" {
		args = append(args, f.Environment)
		fmt.Fprintf(&sb, " AND EXISTS (SELECT 1 FROM issue_environments ie WHERE ie.issue_id = issues.id AND ie.environment = $%d)", len(args))
	}
	if !f.Since.IsZero() {
		args = append(args, f.Since)
		fmt.Fprintf(&sb, " AND issues.last_seen >= $%d", len(args))
	}
	if !f.Until.IsZero() {
		args = append(args, f.Until)
		fmt.Fprintf(&sb, " AND issues.last_seen <= $%d", len(args))
	}
	return sb.String(), args
}

// List возвращает страницу issue проекта с фильтрами и total (без учёта пагинации).
//
// total считается отдельным запросом без JOIN/ORDER BY, а не count(*) OVER()
// в основном запросе. OVER() без PARTITION BY заставляет планировщик
// материализовать, отсортировать и обсчитать ВСЕ подходящие строки прежде
// чем применить LIMIT/OFFSET — при большом числе issue страница 1 стоила бы
// как чтение всего набора, а не 25 строк. Веб-слой показывает total как
// точное число страниц пагинатора («{page} / {totalPages}»,
// internal/web/templates/issues.templ) — то есть точное значение действительно
// нужно, а не просто признак «есть ли ещё одна страница», и убрать его
// вовсе нельзя без изменения интерфейса. Отдельный лёгкий count(*) даёт то
// же число для пагинатора, но не мешает планировщику использовать индекс по
// колонке сортировки и остановиться после LIMIT строк при выборке самих issue.
//
// Счётчик и строки — теперь два отдельных обращения к БД, а не один снимок:
// между ними приём может записать новую issue, и total разойдётся со
// списком на единицу-две на границе страницы. Раньше такого расхождения не
// было вовсе (один запрос — один снимок). Это осознанный размен: цена —
// временная неточность счётчика страниц, самоисправляющаяся при следующем
// открытии списка; цена бездействия — полный скан на каждое открытие.
func (s *Service) List(ctx context.Context, projectID int64, f Filter) ([]Issue, int64, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	order, ok := sortColumns[f.Sort]
	if !ok {
		order = sortColumns[defaultSort]
	}

	where, args := buildIssueFilter(projectID, f)
	offset := (page - 1) * perPage

	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM issues WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("issue: list count: %w", err)
	}
	if int64(offset) >= total {
		// Покрывает и total==0 (offset>=0 всегда), и страницу за пределами
		// данных (?page=<огромное> или фильтр применён между запросами) —
		// отдельной проверки на total==0 не нужно, offset неотрицателен.
		//
		// Раньше count(*) OVER() сидел в ТОМ ЖЕ запросе, что и LIMIT/OFFSET —
		// на пустой странице клиент не получал ни одной строки и,
		// соответственно, ни одного значения total, поэтому total оставался
		// нулём (zero value). Шаблон (issues.templ, pagerPrev) на это
		// опирается буквально: total<=0 читается как «страницы нет, веди на
		// первую», а не как «страница X из total». Раз total теперь считается
		// отдельным запросом ДО пагинации, он был бы больше нуля и на
		// out-of-range странице — это изменило бы поведение пагинатора,
		// поэтому здесь тот же нулевой total воспроизведён явно.
		return nil, 0, nil
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(issueColumnsJoined)
	sb.WriteString(" FROM ")
	sb.WriteString(issueFromJoined)
	sb.WriteString(" WHERE ")
	sb.WriteString(where)
	sb.WriteString(" ORDER BY ")
	sb.WriteString(order)

	rowArgs := append(append([]any{}, args...), perPage, offset)
	fmt.Fprintf(&sb, " LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.pool.Query(ctx, sb.String(), rowArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	defer rows.Close()

	var items []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssueWithAssignee(rows, &i); err != nil {
			return nil, 0, fmt.Errorf("issue: list scan: %w", err)
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	return items, total, nil
}

const exportPageSize = 500

// issueExportSnapshotSafetyLimit — защитный потолок числа групп в снимке id
// StreamForExport (см. её докблок про снимок) — НЕ настраиваемый
// GOTCHA_EXPORT_MAX_ROWS: тот применяет worker.go СНАРУЖИ, останавливая
// обход возвратом ошибки из fn при достижении лимита заявки (тот же путь,
// что и для kind=events, где worker.go на старте проверяет
// MaxRows < eventStreamSafetyLimit=1_000_000, internal/export/worker.go) —
// эта проверка распространяется на ЛЮБОЙ источник, включая issues, так как
// worker оборачивает fn одинаково независимо от Kind. issueExportSnapshotSafetyLimit
// взят РАВНЫМ eventStreamSafetyLimit намеренно: раз MaxRows у ЛЮБОЙ заявки
// уже гарантированно меньше eventStreamSafetyLimit, он тем самым всегда
// меньше и этого потолка — снимок в норме никогда не упрётся в него первым,
// он успевает остановиться по MaxRows (Truncated=true) раньше. Это чистая
// подстраховка на случай аномально большого проекта или неисправности
// внешнего лимита — без верхней границы снимок материализовал бы id ВСЕХ
// подходящих групп разом, прежде чем прочитать хоть одну страницу.
const issueExportSnapshotSafetyLimit = 1_000_000

// ErrExportSnapshotTooLarge возвращается, когда фильтр StreamForExport
// резолвится в больше issueExportSnapshotSafetyLimit групп. Отказ, а не
// тихая обрезка снимка: обрезанный по ЭТОЙ границе (а не по MaxRows заявки,
// который её в норме не достигает — см. докблок issueExportSnapshotSafetyLimit)
// снимок выглядел бы как «выгрузка полна», хотя часть групп в неё не вошла
// бы — тот же принцип, что у ErrTooManyIssues в internal/export.
var ErrExportSnapshotTooLarge = errors.New("issue: export snapshot exceeds safety limit")

// StreamForExport отдаёт группы проекта по снимку их id, постранично, без
// OFFSET, который на многотысячной выгрузке заставлял бы базу перечитывать
// уже отданные строки на каждой следующей странице.
//
// Снимок — id ВСЕХ подходящих фильтру групп резолвится ОДНИМ запросом до
// начала обхода, в порядке last_seen DESC, id DESC (тот же порядок, что и у
// List и раньше был у самого обхода), и этот порядок фиксируется: обход идёт
// строго по накопленному списку id, а НЕ переспрашивает базу постранично по
// last_seen. Так решены сразу два свойства разом:
//
//   - последующая мутация last_seen группы (приём события ПРЯМО ВО ВРЕМЯ
//     обхода) не может ни вытолкнуть её из уже зафиксированного списка, ни
//     задвоить: id в снимке — то же самое множество и тот же порядок, что
//     на момент постановки, поэтому группа, чей last_seen поменялся после
//     снимка, всё равно попадёт в выгрузку ровно один раз, на прежнем месте
//     (аудит 2026-08-27, DEDUP-P1 кластер 5: раньше курсор шёл постранично
//     ПО last_seen, и такая группа уезжала выше курсора и терялась молча,
//     Truncated при этом оставался false);
//   - группа, СОЗДАННАЯ уже ПОСЛЕ снимка, в выгрузку заведомо не попадёт —
//     её id в снимке нет и не может быть. Это осознанная, документированная
//     граница снимка (как у любого моментального среза растущей выборки), а
//     не случайность: экспорт — это состояние на момент постановки, а не
//     живой отчёт, продолжающий расти по мере своего выполнения;
//   - симметрично: группа, УЖЕ бывшая в снимке (её id уже попал в список),
//     чьё поле фильтра (например status) изменится между снимком и чтением
//     ЕЁ страницы, из выгрузки не исчезнет — членство решает id = ANY($1),
//     который WHERE фильтра целиком не переприменяет. Строка при этом придёт
//     с ТЕКУЩИМИ (на момент чтения страницы, не на момент снимка) значениями
//     остальных полей — так что запись может показать, например, статус
//     resolved у группы, отфильтрованной по status=unresolved на постановке.
//     Так и должно быть: снимок фиксирует РЕШЕНИЕ О ЧЛЕНСТВЕ (какие группы
//     войдут в файл и в каком порядке), а не замороженные значения полей
//     каждой из них — иначе, например, ссылка url и title в файле были бы
//     устаревшими уже на момент открытия. Фраза «ровно тем же фильтром, что
//     человек видел на экране» ниже верна для МНОЖЕСТВА групп на момент
//     постановки заявки, а не для каждого поля каждой строки к моменту,
//     когда до неё дошла своя страница.
//
// Ранняя версия этой правки пробовала курсор по issues.id вместо снимка
// (тот тоже не мутирует и решает первый пункт) — но ORDER BY issues.id DESC
// меняет НЕ порядок строк, а их СОСТАВ при усечении по MaxRows заявки: id
// растёт вместе с first_seen, то есть «id DESC» — это «недавно СОЗДАННЫЕ
// группы первыми», и на упирающейся в MaxRows выгрузке (многотысячные
// проекты упираются в неё реально) в файл уезжали самые новые группы вместо
// самых активных — старая группа с большим times_seen и маленьким id из
// усечённого файла пропадала целиком. Снимок этой цены не имеет: он
// упорядочен по last_seen DESC, как и раньше, поэтому усечение по MaxRows
// по-прежнему оставляет в файле самые недавно активные группы.
//
// Цена снимка — фиксированный набор id в памяти на всё время обхода (при
// потолке issueExportSnapshotSafetyLimit — до 8 МБ, int64×1_000_000), а не
// открытая транзакция БД на всю выгрузку (минуты на сотнях тысяч строк, с
// давлением на vacuum самой нагруженной таблицы под REPEATABLE READ) — тот
// более очевидный на первый взгляд вариант сюда осознанно не пошёл.
//
// WHERE снимка строится buildIssueFilter — тем же кодом, что и List: НАБОР
// групп, вошедших в выгрузку, обязан быть ровно тем же, что человек видел на
// экране НА МОМЕНТ ПОСТАНОВКИ заявки (см. третий пункт выше про членство), а
// не отдельной, потенциально разъехавшейся копией условий фильтра.
//
// Обход останавливается на первой ошибке fn: источник выгрузки использует её,
// чтобы прервать поток на потолке строк, не дочитывая выборку до конца.
func (s *Service) StreamForExport(ctx context.Context, projectID int64, f Filter, fn func(Issue) error) error {
	return s.streamForExport(ctx, projectID, f, issueExportSnapshotSafetyLimit, fn)
}

// streamForExport — реализация StreamForExport с потолком снимка ПАРАМЕТРОМ,
// а не жёстко зашитой issueExportSnapshotSafetyLimit: тест на переполнение
// снимка (ErrExportSnapshotTooLarge) иначе стоил бы вставки 1 000 001 строки
// в тестовую БД на каждый прогон — с параметром достаточно нескольких строк
// и маленького лимита (см. internal/issue/streamforexport_internal_test.go,
// package issue, у неё есть доступ к этому неэкспортированному методу).
func (s *Service) streamForExport(ctx context.Context, projectID int64, f Filter, snapshotLimit int, fn func(Issue) error) error {
	where, args := buildIssueFilter(projectID, f)

	snapQ := fmt.Sprintf("SELECT issues.id FROM issues WHERE %s ORDER BY issues.last_seen DESC, issues.id DESC LIMIT %d",
		where, snapshotLimit+1)
	rows, err := s.pool.Query(ctx, snapQ, args...)
	if err != nil {
		return fmt.Errorf("issue: stream for export snapshot: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("issue: stream for export snapshot scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("issue: stream for export snapshot: %w", err)
	}
	if len(ids) > snapshotLimit {
		return ErrExportSnapshotTooLarge
	}

	for start := 0; start < len(ids); start += exportPageSize {
		end := start + exportPageSize
		if end > len(ids) {
			end = len(ids)
		}
		page := ids[start:end]

		// project_id в условии — та же конвенция, что и у ByIDs (query.go):
		// сегодня id в page рождаются из снимка ЭТОЙ ЖЕ функции, отфильтрованного
		// buildIssueFilter по тому же projectID, так что примесь чужого проекта
		// здесь неоткуда взяться — но это единственное место в файле с голым
		// id = ANY без project_id, и полагаться на «id пришли из своего же
		// снимка, честное слово» дороже одного лишнего параметра.
		q := "SELECT " + issueColumnsJoined + " FROM " + issueFromJoined + " WHERE issues.project_id = $1 AND issues.id = ANY($2)"
		pageRows, err := s.pool.Query(ctx, q, projectID, page)
		if err != nil {
			return fmt.Errorf("issue: stream for export page: %w", err)
		}
		byID := make(map[int64]Issue, len(page))
		for pageRows.Next() {
			var it Issue
			if err := scanIssueWithAssignee(pageRows, &it); err != nil {
				pageRows.Close()
				return fmt.Errorf("issue: stream for export scan: %w", err)
			}
			byID[it.ID] = it
		}
		pageRows.Close()
		if err := pageRows.Err(); err != nil {
			return fmt.Errorf("issue: stream for export page: %w", err)
		}

		// Порядок отдачи — порядок СНИМКА (page), не порядок, в котором
		// Postgres вернул строки по id = ANY($1) (он его не гарантирует).
		// Группа, удалённая между снимком и этой страницей, просто
		// отсутствует в byID — пропускаем, не ошибка.
		var batchIDs []int64
		for _, id := range page {
			if _, ok := byID[id]; ok {
				batchIDs = append(batchIDs, id)
			}
		}
		if len(batchIDs) > 0 {
			envs, err := s.environmentsForIssues(ctx, batchIDs)
			if err != nil {
				return err
			}
			for _, id := range batchIDs {
				it := byID[id]
				it.Environments = envs[id]
				byID[id] = it
			}
		}

		// Колбэк вызывается после закрытия rows: он может ходить в ту же базу
		// (источник выгрузки пишет в файл, не в БД, но держать курсор открытым
		// на всю выгрузку всё равно незачем).
		for _, id := range batchIDs {
			if err := fn(byID[id]); err != nil {
				return err
			}
		}
	}
	return nil
}

// IDsForFilter резолвит фильтр списка issues в список id — источнику
// выгрузки событий (kind=events, область «проект с фильтрами») он нужен,
// чтобы ограничить обход ClickHouse тем же набором групп, которые человек
// видел на экране: WHERE строится buildIssueFilter, общим кодом с List и
// StreamForExport, а не отдельной копией условий.
//
// overflow=true — резолвится больше limit групп: обрезать список молча
// нельзя, какие именно группы выпали бы, вызывающая сторона узнать не
// может (см. §8 спеки экспорта), поэтому она обязана вернуть отказ с
// просьбой сузить фильтр вместо тихо неполной выгрузки.
//
// Один запрос с LIMIT limit+1, а не курсорный обход постранично, как в
// StreamForExport: здесь нужен только список id, а не поток строк, и
// ORDER BY last_seen DESC, id DESC уже даёт полный порядок без OFFSET.
func (s *Service) IDsForFilter(ctx context.Context, projectID int64, f Filter, limit int) ([]int64, bool, error) {
	where, args := buildIssueFilter(projectID, f)
	q := fmt.Sprintf("SELECT issues.id FROM issues WHERE %s ORDER BY issues.last_seen DESC, issues.id DESC LIMIT %d",
		where, limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("issue: ids for filter: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("issue: ids for filter scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("issue: ids for filter: %w", err)
	}

	if len(ids) > limit {
		return ids[:limit], true, nil
	}
	return ids, false, nil
}

// environmentsForIssues возвращает окружения набора групп одним запросом —
// вместо N+1 похода в issue_environments на каждую строку страницы.
func (s *Service) environmentsForIssues(ctx context.Context, ids []int64) (map[int64][]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT issue_id, environment FROM issue_environments WHERE issue_id = ANY($1) ORDER BY issue_id, environment", ids)
	if err != nil {
		return nil, fmt.Errorf("issue: environments for issues: %w", err)
	}
	defer rows.Close()

	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var env string
		if err := rows.Scan(&id, &env); err != nil {
			return nil, fmt.Errorf("issue: environments for issues scan: %w", err)
		}
		out[id] = append(out[id], env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: environments for issues: %w", err)
	}
	return out, nil
}

// ActiveSince возвращает issue проекта, у которых last_seen >= since —
// используется spike-воркером алертинга, чтобы ограничить сканирование окна
// правила только недавно активными issue, а не всеми issue проекта.
func (s *Service) ActiveSince(ctx context.Context, projectID int64, since time.Time) ([]Issue, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND last_seen >= $2 ORDER BY last_seen DESC",
		projectID, since)
	if err != nil {
		return nil, fmt.Errorf("issue: active since: %w", err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("issue: active since scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: active since: %w", err)
	}
	return out, nil
}

// CountNewSince возвращает число issue проекта, у которых first_seen >=
// since — «строка состояния» Обзора (задача 7 nav-ia) считает так «новые
// проблемы за сутки», в отличие от ActiveSince выше (та фильтрует по
// last_seen — «недавно шумевшие», включая давно заведённые issue с новым
// событием). COUNT(*) вместо выборки строк: странице нужно только число.
func (s *Service) CountNewSince(ctx context.Context, projectID int64, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= $2",
		projectID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("issue: count new since: %w", err)
	}
	return n, nil
}

// Get возвращает issue по id (с AssigneeEmail) или ErrNotFound.
// ByIDs возвращает группы проекта по списку идентификаторов.
//
// Существует ради spike-детектора: ему нужны заголовки только тех групп, что
// перешагнули порог, а не всех активных. Раньше он забирал из PostgreSQL все
// активные группы проекта, чтобы затем отбросить почти все.
//
// Фильтр по project_id обязателен и здесь: идентификаторы приходят из ответа
// ClickHouse, и доверять им как «уже проверенным» нельзя.
func (s *Service) ByIDs(ctx context.Context, projectID int64, ids []int64) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND id = ANY($2) ORDER BY last_seen DESC",
		projectID, ids)
	if err != nil {
		return nil, fmt.Errorf("issue: by ids: %w", err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("issue: by ids scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: by ids: %w", err)
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, issueID int64) (Issue, error) {
	var i Issue
	row := s.pool.QueryRow(ctx, "SELECT "+issueColumnsJoined+" FROM "+issueFromJoined+" WHERE issues.id = $1", issueID)
	if err := scanIssueWithAssignee(row, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Issue{}, ErrNotFound
		}
		return Issue{}, fmt.Errorf("issue: get: %w", err)
	}
	return i, nil
}

// Exists — есть ли у проекта хоть одна проблема. Для онбординг-галочки нужен
// только факт наличия, поэтому EXISTS вместо List(Filter{}): последний из-за
// count(*) OVER() материализует весь набор проблем проекта на каждый рендер.
func (s *Service) Exists(ctx context.Context, projectID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM issues WHERE project_id = $1)", projectID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// Environments возвращает отсортированный уникальный список environment,
// в которых видели issue проекта (из денормализованной issue_environments).
func (s *Service) Environments(ctx context.Context, projectID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT environment FROM issue_environments WHERE project_id = $1 ORDER BY environment", projectID)
	if err != nil {
		return nil, fmt.Errorf("issue: environments: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("issue: environments scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: environments: %w", err)
	}
	return out, nil
}

// SetStatus меняет статус одного issue. Невалидный статус → ErrInvalidStatus,
// отсутствующий issue → ErrNotFound.
func (s *Service) SetStatus(ctx context.Context, issueID int64, status string) error {
	if !validStatuses[status] {
		return ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx, "UPDATE issues SET status = $1 WHERE id = $2", status, issueID)
	if err != nil {
		return fmt.Errorf("issue: set status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatusBulk меняет статус набора issue, ограниченных проектом projectID;
// id из чужих проектов игнорируются. Возвращает число изменённых строк.
func (s *Service) SetStatusBulk(ctx context.Context, projectID int64, ids []int64, status string) (int64, error) {
	if !validStatuses[status] {
		return 0, ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx,
		"UPDATE issues SET status = $1 WHERE project_id = $2 AND id = ANY($3)",
		status, projectID, ids)
	if err != nil {
		return 0, fmt.Errorf("issue: set status bulk: %w", err)
	}
	return ct.RowsAffected(), nil
}

// Assign назначает issue пользователю; userID == nil снимает назначение.
// Несуществующий issue → ErrNotFound.
func (s *Service) Assign(ctx context.Context, issueID int64, userID *int64) error {
	ct, err := s.pool.Exec(ctx, "UPDATE issues SET assignee_id = $1 WHERE id = $2", userID, issueID)
	if err != nil {
		return fmt.Errorf("issue: assign: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
