package ingest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
)

// validLevels — единственные уровни, которым мы доверяем как есть; всё
// остальное (в т.ч. пустая строка отдельно обрабатывается ниже) каппится до error.
var validLevels = map[string]bool{
	"debug":   true,
	"info":    true,
	"warning": true,
	"error":   true,
	"fatal":   true,
}

// capRunes обрезает s до n рун (недоверенные поля из событий SDK не должны
// раздувать строки/индексы БД без ограничений).
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
	Fingerprint     []string
	Title           string
	Culprit         string
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
		Fingerprint: se.Fingerprint,
		Tags:        map[string]string{},
	}
	if !validLevels[pe.Level] {
		pe.Level = "error"
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
		pe.ContextsJSON = string(se.Contexts)
		pe.TraceID, pe.SpanID = parseTraceIDs(se.Contexts)
	}
	if len(se.Breadcrumbs) > 0 && string(se.Breadcrumbs) != "null" {
		pe.BreadcrumbsJSON = string(se.Breadcrumbs)
	}

	pe.Exceptions = parseExceptions(se.Exception)
	if len(se.Exception) > 0 && string(se.Exception) != "null" {
		pe.StacktraceJSON = string(se.Exception)
	}

	// Title/Culprit строятся уже из каппнутых полей (Message, фреймы).
	pe.Title, pe.Culprit = titleAndCulprit(pe)
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

	var out []fingerprint.Exception
	for _, se := range list {
		ex := fingerprint.Exception{Type: se.Type, Value: se.Value}
		if se.Stacktrace != nil {
			for _, f := range se.Stacktrace.Frames {
				module := f.Module
				if module == "" {
					module = f.Filename
				}
				ex.Frames = append(ex.Frames, fingerprint.Frame{
					Module:   module,
					Function: f.Function,
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
