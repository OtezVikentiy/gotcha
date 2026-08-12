package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// dumpFormat выбирает разметку сборщика renderEventForLLM: markdown (для
// вставки в чат с поддержкой Markdown) или обычный текст.
type dumpFormat int

const (
	dumpMarkdown dumpFormat = iota
	dumpPlain
)

// maxDumpBytes — потолок размера дампа события для LLM (128 КиБ): защита от
// раздутых vars/breadcrumbs, которые иначе забили бы контекст модели целиком.
const maxDumpBytes = 128 << 10

// renderEventForLLM собирает полный контекст события в текст для вставки в LLM.
// Машинный формат, не человеческий показ (время — RFC3339 UTC): дамп читает
// модель, а не пользователь. См. cld/specs/2026-08-12-event-llm-copy-design.md.
func renderEventForLLM(it issue.Issue, ev event.Stored, f dumpFormat) string {
	md := f == dumpMarkdown

	// Тела секций собираются заранее: забор код-блоков в md — один общий на
	// весь дамп, длиной больше самой длинной серии бэктиков во ВСЁМ
	// содержимом (не только внутри самих код-блоков) — иначе бэктики,
	// случайно попавшие в нефенсированную секцию (например, в текст
	// исключения), могли бы визуально слиться с забором соседнего блока.
	exception := ""
	if ev.ExceptionType != "" || ev.ExceptionValue != "" {
		exception = exceptionLine(ev)
	}
	frames := parseStacktraceFrames(ev.Stacktrace)
	stack := framesText(frames)
	req := templates.RequestForDump(ev.Request)
	reqText := ""
	if req != nil {
		reqText = requestText(req)
	}
	contexts := prettyJSON(ev.Contexts)
	tags := ""
	if len(ev.Tags) > 0 {
		tags = tagsText(ev.Tags)
	}
	breadcrumbs := prettyJSON(ev.Breadcrumbs)

	// Забор нужен только в md (в txt код-блоков нет) — не считаем зря.
	var fence string
	if md {
		fence = codeFence(strings.Join([]string{exception, stack, reqText, contexts, tags, breadcrumbs}, "\n"))
	}

	var b strings.Builder
	writeTitle(&b, md, it.Title)
	writeMeta(&b, md, ev)

	writeSection(&b, md, "Exception", exception, false, fence)
	if len(frames) > 0 {
		writeSection(&b, md, "Stack trace", stack, true, fence)
	}
	if req != nil {
		writeSection(&b, md, "Request", reqText, true, fence)
	}
	writeSection(&b, md, "Contexts", contexts, true, fence)
	writeSection(&b, md, "Tags", tags, false, fence)
	writeSection(&b, md, "Breadcrumbs", breadcrumbs, true, fence)

	return capDump(sanitizeControl(b.String()))
}

// writeTitle пишет заголовок дампа: title issue как заголовок первого уровня
// в md, простая строка в txt.
func writeTitle(b *strings.Builder, md bool, title string) {
	if md {
		fmt.Fprintf(b, "# %s\n\n", title)
		return
	}
	fmt.Fprintf(b, "%s\n\n", title)
}

// writeMeta пишет блок метаданных события: level/env/release/server/sdk,
// время в RFC3339 UTC (машинный формат — дамп читает модель, см. докблок
// renderEventForLLM), event_id, trace_id. Пустые поля опускаются.
func writeMeta(b *strings.Builder, md bool, ev event.Stored) {
	rows := []struct {
		label string
		val   string
	}{
		{"level", ev.Level},
		{"env", ev.Environment},
		{"release", ev.Release},
		{"server", ev.ServerName},
		{"sdk", ev.SDK},
		{"time", ev.Timestamp.UTC().Format(time.RFC3339)},
		{"event_id", ev.ID},
		{"trace_id", ev.TraceID},
	}
	for _, r := range rows {
		if r.val == "" {
			continue
		}
		if md {
			fmt.Fprintf(b, "- %s: %s\n", r.label, r.val)
		} else {
			fmt.Fprintf(b, "%s: %s\n", r.label, r.val)
		}
	}
	b.WriteString("\n")
}

// writeSection добавляет секцию с заголовком title и телом body. Пустое
// body — секция целиком пропускается (правило «пустые секции опускать»).
// В md заголовок — "## title", тело в код-блоке при code==true (общий на
// весь дамп fence, см. renderEventForLLM). В txt — "TITLE:" заглавными,
// без разметки.
func writeSection(b *strings.Builder, md bool, title, body string, code bool, fence string) {
	if body == "" {
		return
	}
	if md {
		fmt.Fprintf(b, "## %s\n\n", title)
		if code {
			b.WriteString(fence)
			b.WriteString("\n")
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(fence)
			b.WriteString("\n\n")
		} else {
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		return
	}
	fmt.Fprintf(b, "%s:\n", strings.ToUpper(title))
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// codeFence возвращает строку из бэктиков длиннее самой длинной серии
// бэктиков внутри body (минимум 3) — иначе забор внутри содержимого
// закрыл бы код-блок раньше времени.
func codeFence(body string) string {
	longest := 0
	run := 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// exceptionLine форматирует строку исключения "Type: Value" (пустые части
// опускаются вместе с разделителем).
func exceptionLine(ev event.Stored) string {
	switch {
	case ev.ExceptionType != "" && ev.ExceptionValue != "":
		return fmt.Sprintf("%s: %s", ev.ExceptionType, ev.ExceptionValue)
	case ev.ExceptionType != "":
		return ev.ExceptionType
	default:
		return ev.ExceptionValue
	}
}

// framesText форматирует кадры стектрейса, по одному кадру — строка
// "Filename:Lineno  Function" (+" [Module]", если Module непусто), следом
// строка с ContextLine, если она непуста. Порядок сохраняется как отдал
// parseStacktraceFrames (не переставлять — global-constraints.md).
func framesText(frames []templates.Frame) string {
	var b strings.Builder
	for i, f := range frames {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s:%d  %s", f.Filename, f.Lineno, f.Function)
		if f.Module != "" {
			fmt.Fprintf(&b, " [%s]", f.Module)
		}
		if f.ContextLine != "" {
			b.WriteString("\n")
			b.WriteString(f.ContextLine)
		}
	}
	return b.String()
}

// requestText форматирует HTTP-запрос: строка "METHOD URL", затем query,
// headers, body — каждый непустой блок на своей строке/строках.
func requestText(r *templates.RequestDump) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Method, r.URL)
	if len(r.Query) > 0 {
		b.WriteString("\n\nQuery:")
		for _, kv := range r.Query {
			fmt.Fprintf(&b, "\n  %s=%s", kv.Key, kv.Val)
		}
	}
	if len(r.Headers) > 0 {
		b.WriteString("\n\nHeaders:")
		for _, kv := range r.Headers {
			fmt.Fprintf(&b, "\n  %s: %s", kv.Key, kv.Val)
		}
	}
	if r.Body != "" {
		b.WriteString("\n\nBody:\n")
		b.WriteString(r.Body)
	}
	return b.String()
}

// tagsText форматирует теги события как "k=v", по одному на строку,
// отсортированные по ключу для детерминированного вывода.
func tagsText(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s=%s", k, tags[k])
	}
	return b.String()
}

// prettyJSON переформатирует raw с отступами для читаемости. Пусто (""),
// "{}", "null" или невалидный JSON — пустая строка (секция опускается).
func prettyJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return ""
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// sanitizeControl заменяет недопустимые control-руны (кроме \n, \t) на
// пробел — дамп мог бы иначе унести управляющие символы из vars/body в
// текст, отдаваемый LLM.
func sanitizeControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// capDumpMarker — хвост, добавляемый при обрезке дампа сверх maxDumpBytes.
// По-английски, как и весь дамп (заголовки секций Exception/Stack trace и т.д.):
// это машинный текст для LLM, а не элемент локализованного интерфейса.
const capDumpMarker = "\n…[truncated]"

// capDump обрезает s по границе руны, если он превышает maxDumpBytes, и
// добавляет маркер обрезки (не рвёт UTF-8 посередине руны).
func capDump(s string) string {
	if len(s) <= maxDumpBytes {
		return s
	}
	limit := maxDumpBytes - len(capDumpMarker)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !isRuneBoundary(s, limit) {
		limit--
	}
	return s[:limit] + capDumpMarker
}

// isRuneBoundary сообщает, начинается ли в позиции i руна (не находится ли
// i внутри многобайтовой UTF-8 последовательности).
func isRuneBoundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}
