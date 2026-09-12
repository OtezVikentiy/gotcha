package profile

import (
	"strconv"
	"strings"
	"testing"
)

// стек должен быть уникальным по idx, иначе Add схлопнет несколько вызовов в одну строку.
func sample1(idx int) Profile {
	return Profile{
		Type: "cpu",
		Samples: []Sample{{
			Stack: []Frame{{Function: "fn" + strconv.Itoa(idx)}},
			Value: 1,
		}},
	}
}

func TestWriterSaturationEmpty(t *testing.T) {
	w := NewWriter(nil)
	if got := w.Saturation(); got != 0 {
		t.Fatalf("Saturation() = %v на пустом буфере, want 0", got)
	}
}

func TestWriterSaturationRowCap(t *testing.T) {
	w := NewWriter(nil)
	w.maxBuf = 4
	w.maxBufBytes = 1 << 30 // байтовый потолок недостижим
	for i := 0; i < 4; i++ {
		w.Add(1, sample1(i))
	}
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по строкам, want >= 1", got)
	}
}

// trimLocked всегда оставляет хотя бы одну строку, даже тяжелее потолка.
func TestWriterSaturationByteCap(t *testing.T) {
	w := NewWriter(nil)
	w.maxBuf = 200000
	w.maxBufBytes = 50 // меньше веса одной строки ниже
	p := Profile{
		Type: "cpu",
		Samples: []Sample{{
			Stack: []Frame{{Function: strings.Repeat("f", 500)}},
			Value: 1,
		}},
	}
	w.Add(1, p)
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по байтам, want >= 1", got)
	}
}

func TestWriterSaturationPartial(t *testing.T) {
	w := NewWriter(nil)
	w.maxBuf = 10
	w.maxBufBytes = 1 << 30
	for i := 0; i < 3; i++ {
		w.Add(1, sample1(i))
	}
	got := w.Saturation()
	if got <= 0 || got >= 1 {
		t.Fatalf("Saturation() = %v при частичном заполнении, want строго между 0 и 1", got)
	}
}

// потолок 0 или отрицательный значит «лимитом не ограничены» — вклад 0, не деление на ноль.
func TestBufSaturationZeroDenominatorIsUnbounded(t *testing.T) {
	if got := bufSaturation(5, 0); got != 0 {
		t.Fatalf("bufSaturation(5, 0) = %v, want 0", got)
	}
	if got := bufSaturation(5, -1); got != 0 {
		t.Fatalf("bufSaturation(5, -1) = %v, want 0", got)
	}
}
