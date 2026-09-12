package trace

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"time"
)

type Span struct {
	SpanID       string
	ParentSpanID string
	Op           string
	Description  string
	Start        time.Time
	End          time.Time
	Status       string
	Data         map[string]any // сериализуется в CH-колонку data (JSON)
}

// корневой спан пишется В ОБЕ таблицы: в transactions (для перцентилей) и в
// spans (иначе waterfall останется без корня).
type Transaction struct {
	TraceID string
	SpanID  string

	Name   string
	Op     string
	Status string

	Start time.Time
	End   time.Time

	Environment string
	Release     string
	ServerName  string
	UserID      string

	Tags   map[string]string
	Spans  []Span // дочерние спаны (без корневого)
	Source string // "sentry" | "otlp"

	// ms-vitals в миллисекундах; nil допустим — в CH уезжает пустой Map (см. SpanWriter.Add).
	Measurements map[string]float64
}

// 0, если End <= Start (SDK присылает и такое); насыщение на MaxUint32 — колонка UInt32.
func (t Transaction) DurationUS() uint32 {
	return durationUS(t.Start, t.End)
}

func (s Span) DurationUS() uint32 {
	return durationUS(s.Start, s.End)
}

func durationUS(start, end time.Time) uint32 {
	d := end.Sub(start)
	if d <= 0 {
		return 0
	}
	us := d.Microseconds()
	if us > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(us)
}

// нормализация description (SQL/URL) сюда не входит — это делают детекторы,
// здесь хешируется уже готовое описание.
func DescriptionHash(op, description string) uint64 {
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0}) // разделитель: ("ab","c") не должно совпасть с ("a","bc")
	h.Write([]byte(description))
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}
