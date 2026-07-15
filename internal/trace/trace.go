// Package trace — домен распределённого трейсинга: модель транзакции и её
// спанов, запись в ClickHouse (см. writer.go) и детерминированное
// семплирование трейсов (см. sample.go).
package trace

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"time"
)

// Span — дочерний спан транзакции: одна операция внутри трейса (запрос в БД,
// исходящий HTTP и т.п.). Пишется в CH-таблицу spans.
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

// Transaction — корневой спан трейса вместе с дочерними. Корневой спан
// пишется В ОБЕ таблицы: в transactions (для перцентилей) и в spans (иначе
// waterfall останется без корня).
type Transaction struct {
	TraceID string
	SpanID  string

	Name   string // имя транзакции (эндпойнт)
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
}

// DurationUS — длительность транзакции в микросекундах; 0, если End <= Start
// (SDK присылает и такое), с насыщением на MaxUint32 — колонка UInt32.
func (t Transaction) DurationUS() uint32 {
	return durationUS(t.Start, t.End)
}

// DurationUS — длительность спана в микросекундах; правила те же, что у
// Transaction.DurationUS.
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

// DescriptionHash — стабильный хеш пары (op, description) для группировки
// одинаковых операций. Нормализация описания (SQL/URL) сюда НЕ входит: она
// живёт в детекторах, здесь хешируется уже готовое описание.
func DescriptionHash(op, description string) uint64 {
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0}) // разделитель: ("ab","c") не должно совпасть с ("a","bc")
	h.Write([]byte(description))
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}
