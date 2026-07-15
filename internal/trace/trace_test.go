package trace_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestTransactionDurationUS(t *testing.T) {
	start := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  uint32
	}{
		{"1500ms", start, start.Add(1500 * time.Millisecond), 1_500_000},
		{"end == start", start, start, 0},
		{"end before start", start, start.Add(-time.Second), 0},
		{"zero end", start, time.Time{}, 0},
		{"sub-microsecond", start, start.Add(500 * time.Nanosecond), 0},
		{"overflow clamped", start, start.Add(2 * time.Hour), math.MaxUint32},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := trace.Transaction{Start: tc.start, End: tc.end}
			if got := tr.DurationUS(); got != tc.want {
				t.Fatalf("Transaction.DurationUS() = %d, want %d", got, tc.want)
			}
			s := trace.Span{Start: tc.start, End: tc.end}
			if got := s.DurationUS(); got != tc.want {
				t.Fatalf("Span.DurationUS() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDescriptionHashIsStable(t *testing.T) {
	a := trace.DescriptionHash("db.query", "SELECT * FROM users WHERE id = 1")
	b := trace.DescriptionHash("db.query", "SELECT * FROM users WHERE id = 1")
	if a != b {
		t.Fatalf("hash not stable: %d != %d", a, b)
	}
	if a == 0 {
		t.Fatal("hash = 0 for non-empty description")
	}
}

func TestDescriptionHashDependsOnOpAndDescription(t *testing.T) {
	base := trace.DescriptionHash("db.query", "SELECT 1")
	if got := trace.DescriptionHash("http.client", "SELECT 1"); got == base {
		t.Fatal("hash ignores op")
	}
	if got := trace.DescriptionHash("db.query", "SELECT 2"); got == base {
		t.Fatal("hash ignores description")
	}
	// Разделитель между op и description: склейка не должна коллизировать.
	if trace.DescriptionHash("ab", "c") == trace.DescriptionHash("a", "bc") {
		t.Fatal("op/description boundary collision")
	}
}

func TestKeepEdgeRates(t *testing.T) {
	ids := []string{"a", "b", "deadbeefdeadbeefdeadbeefdeadbeef", ""}
	for _, id := range ids {
		for _, rate := range []float64{0, -1, math.NaN()} {
			if trace.Keep(id, rate) {
				t.Fatalf("Keep(%q, %v) = true, want false", id, rate)
			}
		}
		for _, rate := range []float64{1, 1.5} {
			if !trace.Keep(id, rate) {
				t.Fatalf("Keep(%q, %v) = false, want true", id, rate)
			}
		}
	}
}

func TestKeepIsDeterministic(t *testing.T) {
	const id = "4bf92f3577b34da6a3ce929d0e0e4736"
	want := trace.Keep(id, 0.5)
	for i := 0; i < 100; i++ {
		if got := trace.Keep(id, 0.5); got != want {
			t.Fatalf("Keep(%q, 0.5) flapped: %v then %v", id, want, got)
		}
	}
}

// TestKeepIgnoresTraceIDCase: регистр hex'а не должен влиять на решение. Иначе
// один и тот же трейс, чей id один источник закодировал в верхнем регистре
// (OTLP везёт его сырыми байтами), а другой — в нижнем, окажется наполовину
// сохранён, наполовину выброшен. При каком-нибудь rate это обязано «выстрелить»
// на любом наборе id, поэтому перебираем и id, и rate.
func TestKeepIgnoresTraceIDCase(t *testing.T) {
	for i := 0; i < 200; i++ {
		lower := fmt.Sprintf("%032x", i*2654435761)
		upper := strings.ToUpper(lower)
		for _, rate := range []float64{0.1, 0.5, 0.9} {
			if trace.Keep(lower, rate) != trace.Keep(upper, rate) {
				t.Fatalf("Keep(%q, %v) = %v, but Keep(%q, %v) = %v — the same trace would be half-kept",
					lower, rate, trace.Keep(lower, rate), upper, rate, trace.Keep(upper, rate))
			}
		}
	}
	// Пробелы по краям (кривой SDK) тоже не должны менять решение.
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("%032x", i*2654435761)
		if trace.Keep(" "+id+"\n", 0.5) != trace.Keep(id, 0.5) {
			t.Fatalf("Keep is sensitive to surrounding whitespace for %q", id)
		}
	}
}

func TestKeepSplitsTraceIDsAtHalfRate(t *testing.T) {
	const n = 10000
	kept := 0
	for i := 0; i < n; i++ {
		if trace.Keep(fmt.Sprintf("%032x", i*2654435761), 0.5) {
			kept++
		}
	}
	frac := float64(kept) / float64(n)
	if frac < 0.45 || frac > 0.55 {
		t.Fatalf("kept fraction = %.3f, want within [0.45, 0.55]", frac)
	}
}

func TestKeepIsNotAllOrNothing(t *testing.T) {
	// Разные trace_id должны получать разные решения при одном и том же rate.
	var sawKeep, sawDrop bool
	for i := 0; i < 100; i++ {
		if trace.Keep(fmt.Sprintf("trace-%d", i), 0.5) {
			sawKeep = true
		} else {
			sawDrop = true
		}
	}
	if !sawKeep || !sawDrop {
		t.Fatalf("Keep at rate 0.5: sawKeep=%v sawDrop=%v, want both", sawKeep, sawDrop)
	}
}
