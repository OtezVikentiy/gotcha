package selfmetrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Type string

const (
	Counter Type = "counter" // монотонно растёт (потери, отказы)
	Gauge   Type = "gauge"   // текущее значение (глубина буфера)
)

type entry struct {
	name   string
	help   string
	typ    Type
	labels map[string]string
	value  func() float64
}

// нулевое значение готово к работе.
type Registry struct {
	mu      sync.Mutex
	entries []entry
}

// value вызывается на каждый скрап — обязана быть дешёвой и не ходить в БД.
func (r *Registry) Add(typ Type, name, help string, labels map[string]string, value func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{name: name, help: help, typ: typ, labels: labels, value: value})
}

func (r *Registry) AddInt(typ Type, name, help string, labels map[string]string, value func() int64) {
	r.Add(typ, name, help, labels, func() float64 { return float64(value()) })
}

// метрики с одинаковым именем (разные метки) группируются под одним блоком HELP/TYPE.
func (r *Registry) Gather() string {
	r.mu.Lock()
	snapshot := make([]entry, len(r.entries))
	copy(snapshot, r.entries)
	r.mu.Unlock()

	sort.SliceStable(snapshot, func(i, j int) bool { return snapshot[i].name < snapshot[j].name })

	var sb strings.Builder
	prev := ""
	for _, e := range snapshot {
		if e.name != prev {
			sb.WriteString("# HELP ")
			sb.WriteString(e.name)
			sb.WriteByte(' ')
			sb.WriteString(escapeHelp(e.help))
			sb.WriteByte('\n')
			sb.WriteString("# TYPE ")
			sb.WriteString(e.name)
			sb.WriteByte(' ')
			sb.WriteString(string(e.typ))
			sb.WriteByte('\n')
			prev = e.name
		}
		sb.WriteString(e.name)
		writeLabels(&sb, e.labels)
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatFloat(e.value(), 'g', -1, 64))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ни одного обращения к БД: должен отвечать и тогда, когда БД недоступна.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Gather()))
	}
}

func writeLabels(sb *strings.Builder, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys) // стабильный вывод: одинаковый скрап — одинаковый текст
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabel(labels[k]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
}

// в HELP экранируются обратный слэш и перевод строки.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// в значении метки дополнительно экранируется кавычка.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
