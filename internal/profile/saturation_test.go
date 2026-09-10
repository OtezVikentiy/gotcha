package profile

import (
	"strconv"
	"strings"
	"testing"
)

// sample1 строит профиль с одним сэмплом на уникальном стеке (различается по
// idx), чтобы схлопывание одинаковых стеков внутри Add не сжало несколько
// вызовов в одну строку.
func sample1(idx int) Profile {
	return Profile{
		Type: "cpu",
		Samples: []Sample{{
			Stack: []Frame{{Function: "fn" + strconv.Itoa(idx)}},
			Value: 1,
		}},
	}
}

// TestWriterSaturationEmpty — пустой буфер не насыщен.
func TestWriterSaturationEmpty(t *testing.T) {
	w := NewWriter(nil)
	if got := w.Saturation(); got != 0 {
		t.Fatalf("Saturation() = %v на пустом буфере, want 0", got)
	}
}

// TestWriterSaturationRowCap — упор в потолок ПО СТРОКАМ при незанятом
// байтовом плече.
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

// TestWriterSaturationByteCap — упор в потолок ПО БАЙТАМ при незанятом
// счётном плече: одна строка тяжелее потолка сама по себе не вычищается
// (trimLocked всегда оставляет хотя бы одну), поэтому байтовое плечо должно
// быть видно даже при единственной строке в буфере.
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

// TestWriterSaturationPartial — частичное заполнение даёт значение строго
// между 0 и 1.
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

// TestBufSaturationZeroDenominatorIsUnbounded — потолок, выключенный нулём
// (или отрицательным значением), означает «этим лимитом не ограничены»: вклад
// в Saturation обязан быть 0, а не деление на ноль/панику/+Inf.
func TestBufSaturationZeroDenominatorIsUnbounded(t *testing.T) {
	if got := bufSaturation(5, 0); got != 0 {
		t.Fatalf("bufSaturation(5, 0) = %v, want 0", got)
	}
	if got := bufSaturation(5, -1); got != 0 {
		t.Fatalf("bufSaturation(5, -1) = %v, want 0", got)
	}
}
