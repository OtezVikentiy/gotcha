package ingest

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// maxJSONBlock — потолок сырого JSON-блока события (contexts/breadcrumbs/
// request/exception). 256 КиБ с запасом покрывают легитимный стектрейс с исходным
// контекстом и большой request-интерфейс; всё, что крупнее, — злоупотребление.
const maxJSONBlock = 256 << 10

// Потолки разбора exception-интерфейса. Число исключений и кадров задаёт КЛИЕНТ,
// а строки кадров попадают в fingerprint.Frame и дальше в вычисление отпечатка —
// без потолков 10 МиБ тела разворачивались в ~130 МБ структур (та же механика,
// что у кадров профиля: ограничено количество ЭЛЕМЕНТОВ, но не размер строк).
const (
	maxExceptions      = 32
	maxFramesPerExc    = 1024
	maxExceptionField  = 1024 // type/value одного исключения
	maxFrameFieldRunes = 512  // module/function одного кадра
	maxTitleRunes      = 1024
	// Пользовательский fingerprint: массив целиком уходит в ключ группировки и
	// оседает в колонке issues, поэтому ограничен и по числу элементов, и по
	// длине каждого — как и всё остальное, что приходит из события.
	maxFingerprintParts = 32
	maxFingerprintPart  = 256
)

// capJSONBlock возвращает блок, если он влезает в maxJSONBlock, иначе ОТБРАСЫВАЕТ
// его целиком. Именно отбрасывает, а не обрезает: обрезанный JSON невалиден, его
// не разберёт ни скрубер (тогда ПДн уехали бы сырыми — ScrubJSON возвращает вход
// как есть на невалидном JSON), ни отрисовка детали issue.
//
// Без этого капа четыре сырых блока были единственными строками события вне
// дисциплины capRunes, а буферы ниже по конвейеру считают СТРОКИ, а не байты:
// очередь пайплайна 1000 задач и батч событий 10000 строк по 10 МиБ каждая дают
// потолок памяти в десятки гигабайт от потока сжатых до килобайтов запросов.
func capJSONBlock(raw []byte, field string) string {
	if len(raw) <= maxJSONBlock {
		return string(raw)
	}
	slog.Warn("event json block too large, dropped",
		"field", field, "bytes", len(raw), "limit", maxJSONBlock)
	return ""
}

// capRunes обрезает s до n рун (недоверенные поля из событий SDK не должны
// раздувать строки/индексы БД без ограничений) и вычищает NUL (0x00).
//
// NUL в JSON легален, а в text-колонках PostgreSQL — нет (SQLSTATE
// 22021): событие с NUL в любом строковом поле принималось приёмом с 200 и
// погибало на issue-upsert — клиент считал его доставленным, данные терялись.
// Все недоверенные строки всех поверхностей приёма (sentry/envelope/OTLP/
// pprof) проходят через эту функцию, поэтому вычистка закреплена здесь.
func capRunes(s string, n int) string {
	// IndexByte — дешёвый просмотр без аллокаций; ReplaceAll платится только
	// строками, реально несущими NUL.
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	// Быстрый путь без единой аллокации. В UTF-8 байт всегда не меньше, чем рун,
	// поэтому len(s) <= n гарантирует, что рун тоже не больше n. Проверка стоит
	// ДО []rune(s) намеренно: подавляющее большинство строк приёма короткие, и
	// раньше каждая из них платила копией всей строки в срез рун — на индексных
	// профилях это давало миллионы копий и гигабайты аллокаций.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// capFingerprint ограничивает пользовательский fingerprint по числу элементов и
// длине каждого. Массив приходит из события как есть и целиком уходит в ключ
// группировки (fingerprint.Compute склеивает его через \x00), а оттуда — в
// колонку issues. Без капа одно событие могло нести килобайты в ключе группы,
// который потом хранится, индексируется и сравнивается на каждом приёме.
//
// Кап НЕ защищает от размножения issue: уникальный отпечаток на каждое событие
// по-прежнему создаёт новый issue, а троттлинг алертов ключуется парой
// (issue_id, rule_id) и у нового issue не срабатывает никогда. Это отдельная
// задача — потолок уведомлений на проект.
func capFingerprint(fp []string) []string {
	if len(fp) == 0 {
		return nil
	}
	if len(fp) > maxFingerprintParts {
		fp = fp[:maxFingerprintParts]
	}
	out := make([]string, len(fp))
	for i, p := range fp {
		out[i] = capRunes(p, maxFingerprintPart)
	}
	return out
}

// normalizeID приводит trace_id/span_id/parent_span_id к каноническому виду:
// обрезка пробелов, нижний регистр, кап длины. Регистр hex'а выбирает тот, кто
// его кодирует (OTLP везёт trace id 16 сырыми байтами), поэтому один и тот же
// трейс от разных источников должен храниться одинаково — иначе развалятся и
// join spans↔transactions по trace_id, и детерминированное семплирование
// (см. trace.Keep).
func normalizeID(s string, n int) string {
	return capRunes(strings.ToLower(strings.TrimSpace(s)), n)
}

// ParsedEvent — нормализованное Sentry-событие, готовое для пайплайна.
type ParsedEvent struct {
	EventID         string
	Timestamp       time.Time
	Level           string
	Message         string
	Exceptions      []fingerprint.Exception
	StacktraceJSON  string
	Environment     string
	Release         string
	ServerName      string
	SDK             string
	UserID          string
	UserIP          string
	UserEmail       string
	Tags            map[string]string
	ContextsJSON    string
	BreadcrumbsJSON string
	// RequestJSON — Sentry-интерфейс request верхнего уровня (method/url/
	// query_string/data/headers/cookies). Хранится как есть (после скраба PII в
	// пайплайне), парсится к показу на детали issue.
	RequestJSON string
	Fingerprint []string
	Title       string
	Culprit     string
	// TraceID/SpanID — из contexts.trace: SDK кладут их в событие, когда
	// включён трейсинг. Едут в одноимённые колонки events и связывают ошибку
	// с транзакцией (пустые, если трейсинга нет).
	TraceID string
	SpanID  string
}

type sentryFrame struct {
	Function string `json:"function"`
	Module   string `json:"module"`
	Filename string `json:"filename"`
	InApp    *bool  `json:"in_app"`
}

type sentryException struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace *struct {
		Frames []sentryFrame `json:"frames"`
	} `json:"stacktrace"`
}

type sentryEvent struct {
	EventID     string          `json:"event_id"`
	Timestamp   json.RawMessage `json:"timestamp"`
	Level       string          `json:"level"`
	Message     json.RawMessage `json:"message"`
	Logentry    json.RawMessage `json:"logentry"`
	Exception   json.RawMessage `json:"exception"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	ServerName  string          `json:"server_name"`
	SDK         *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"sdk"`
	User *struct {
		ID    string `json:"id"`
		IP    string `json:"ip_address"`
		Email string `json:"email"`
	} `json:"user"`
	Tags        json.RawMessage `json:"tags"`
	Contexts    json.RawMessage `json:"contexts"`
	Breadcrumbs json.RawMessage `json:"breadcrumbs"`
	Request     json.RawMessage `json:"request"`
	Fingerprint []string        `json:"fingerprint"`
}

// ParseEvent разбирает Sentry event JSON, терпимо к вариациям SDK:
// timestamp числом или ISO-строкой, message строкой или объектом,
// tags map'ой или массивом пар, exception объектом {values:[...]} или массивом.
func ParseEvent(raw []byte) (*ParsedEvent, error) {
	var se sentryEvent
	if err := json.Unmarshal(raw, &se); err != nil {
		return nil, fmt.Errorf("ingest: event json: %w", err)
	}

	pe := &ParsedEvent{
		Level:       se.Level,
		Environment: capRunes(se.Environment, 200),
		Release:     capRunes(se.Release, 200),
		ServerName:  capRunes(se.ServerName, 200),
		Fingerprint: capFingerprint(se.Fingerprint),
		Tags:        map[string]string{},
	}
	if !issue.IsValidLevel(pe.Level) {
		pe.Level = issue.LevelError
	}

	if id, err := uuid.Parse(se.EventID); err == nil {
		pe.EventID = id.String()
	} else {
		pe.EventID = uuid.New().String()
	}

	pe.Timestamp = parseTimestamp(se.Timestamp)

	pe.Message = parseMessage(se.Message)
	if pe.Message == "" {
		pe.Message = parseMessage(se.Logentry)
	}
	pe.Message = capRunes(pe.Message, 8192)

	if se.SDK != nil {
		pe.SDK = capRunes(se.SDK.Name+"/"+se.SDK.Version, 200)
	}
	if se.User != nil {
		// user_* — недоверенные строки события, каппим по длине как прочие поля
		// (Environment/Release/ServerName выше), чтобы не раздувать колонки events.
		pe.UserID = capRunes(se.User.ID, 200)
		pe.UserIP = capRunes(se.User.IP, 200)
		pe.UserEmail = capRunes(se.User.Email, 200)
	}
	parseTags(se.Tags, pe.Tags)
	pe.Tags = capTags(pe.Tags)
	if len(se.Contexts) > 0 && string(se.Contexts) != "null" {
		pe.ContextsJSON = capJSONBlock(se.Contexts, "contexts")
		pe.TraceID, pe.SpanID = parseTraceIDs(se.Contexts)
	}
	if len(se.Breadcrumbs) > 0 && string(se.Breadcrumbs) != "null" {
		pe.BreadcrumbsJSON = capJSONBlock(se.Breadcrumbs, "breadcrumbs")
	}
	if len(se.Request) > 0 && string(se.Request) != "null" {
		pe.RequestJSON = capJSONBlock(se.Request, "request")
	}

	pe.Exceptions = parseExceptions(se.Exception)
	if len(se.Exception) > 0 && string(se.Exception) != "null" {
		pe.StacktraceJSON = capJSONBlock(se.Exception, "exception")
	}

	// Title/Culprit строятся уже из каппнутых полей (Message, фреймы).
	pe.Title, pe.Culprit = titleAndCulprit(pe)
	pe.Title = capRunes(pe.Title, maxTitleRunes)
	pe.Culprit = capRunes(pe.Culprit, 200)
	return pe, nil
}

// capTags ограничивает недоверенные теги: не более 64 штук, ключ до 64 рун,
// значение до 256 рун (лишнее обрезается, а не отбрасывается целиком).
// Порядок выбора тегов детерминирован: первые 64 в отсортированном порядке.
func capTags(tags map[string]string) map[string]string {
	if len(tags) <= 64 {
		out := make(map[string]string, len(tags))
		for k, v := range tags {
			out[capRunes(k, 64)] = capRunes(v, 256)
		}
		return out
	}

	// Сортируем ключи для детерминированного выбора.
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Берем первые 64 ключей в отсортированном порядке.
	out := make(map[string]string, 64)
	for i := 0; i < 64 && i < len(keys); i++ {
		k := keys[i]
		out[capRunes(k, 64)] = capRunes(tags[k], 256)
	}
	return out
}

// parseTraceIDs достаёт contexts.trace.trace_id/span_id события. Битые или
// отсутствующие contexts — не ошибка события: просто нет связи с трейсом.
func parseTraceIDs(contexts json.RawMessage) (traceID, spanID string) {
	var c struct {
		Trace *struct {
			TraceID string `json:"trace_id"`
			SpanID  string `json:"span_id"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(contexts, &c); err != nil || c.Trace == nil {
		return "", ""
	}
	return normalizeID(c.Trace.TraceID, maxTraceID), normalizeID(c.Trace.SpanID, maxSpanID)
}

// parseTimestamp разбирает timestamp события (unix-число или RFC3339-строка) и
// ПОДТЯГИВАЕТ его к окну хранения [now-90d, now+1d] (см. timestamp.go): events
// партиционируется по toYYYYMM(timestamp), и пачка событий с timestamp'ами из
// сотни разных месяцев иначе заклинила бы вставку целиком. Отсутствующий или
// нечитаемый timestamp — «сейчас», как и раньше.
func parseTimestamp(raw json.RawMessage) time.Time {
	now := time.Now().UTC()
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f > 0 {
		sec := int64(f)
		return clampToRetentionWindow(time.Unix(sec, int64((f-float64(sec))*1e9)).UTC(), now)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return clampToRetentionWindow(ts.UTC(), now)
		}
	}
	return now
}

func parseMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Formatted != "" {
			return obj.Formatted
		}
		return obj.Message
	}
	return ""
}

func parseTags(raw json.RawMessage, out map[string]string) {
	if len(raw) == 0 {
		return
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil {
		for k, v := range m {
			out[k] = v
		}
		return
	}
	var pairs [][2]string
	if err := json.Unmarshal(raw, &pairs); err == nil {
		for _, p := range pairs {
			out[p[0]] = p[1]
		}
	}
}

func parseExceptions(raw json.RawMessage) []fingerprint.Exception {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var wrapper struct {
		Values []sentryException `json:"values"`
	}
	var list []sentryException
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Values) > 0 {
		list = wrapper.Values
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}

	if len(list) > maxExceptions {
		list = list[:maxExceptions]
	}
	out := make([]fingerprint.Exception, 0, len(list))
	for _, se := range list {
		ex := fingerprint.Exception{
			Type:  capRunes(se.Type, maxExceptionField),
			Value: capRunes(se.Value, maxExceptionField),
		}
		if se.Stacktrace != nil {
			frames := se.Stacktrace.Frames
			if len(frames) > maxFramesPerExc {
				frames = frames[:maxFramesPerExc]
			}
			ex.Frames = make([]fingerprint.Frame, 0, len(frames))
			for _, f := range frames {
				module := f.Module
				if module == "" {
					module = f.Filename
				}
				ex.Frames = append(ex.Frames, fingerprint.Frame{
					Module:   capRunes(module, maxFrameFieldRunes),
					Function: capRunes(f.Function, maxFrameFieldRunes),
					InApp:    f.InApp != nil && *f.InApp,
				})
			}
		}
		out = append(out, ex)
	}
	return out
}

func titleAndCulprit(pe *ParsedEvent) (title, culprit string) {
	if n := len(pe.Exceptions); n > 0 {
		ex := pe.Exceptions[n-1]
		title = ex.Type
		if ex.Value != "" {
			title += ": " + ex.Value
		}
		// Фреймы Sentry — от старых к новым; верх стека последний.
		frames := ex.Frames
		for i := len(frames) - 1; i >= 0; i-- {
			if frames[i].InApp {
				culprit = frames[i].Module + "." + frames[i].Function
				break
			}
		}
		if culprit == "" && len(frames) > 0 {
			last := frames[len(frames)-1]
			culprit = last.Module + "." + last.Function
		}
	} else {
		title, _, _ = strings.Cut(pe.Message, "\n")
	}
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200])
	}
	return title, culprit
}
