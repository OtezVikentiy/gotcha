package event

import (
	"strings"
	"testing"
)

// TestBatcherSaturationEmpty — пустой буфер не насыщен.
func TestBatcherSaturationEmpty(t *testing.T) {
	b := NewBatcher(nil)
	if got := b.Saturation(); got != 0 {
		t.Fatalf("Saturation() = %v на пустом буфере, want 0", got)
	}
}

// TestBatcherSaturationRowCap — упор в потолок ПО СТРОКАМ при незанятом
// байтовом плече: заполненность должна дойти до потолка независимо от
// байтового плеча.
func TestBatcherSaturationRowCap(t *testing.T) {
	b := NewBatcher(nil)
	b.maxBuf = 4
	b.maxBufBytes = 1 << 30 // байтовый потолок недостижим
	for i := 0; i < 4; i++ {
		b.Add(Event{ID: "e", Message: "x"})
	}
	if got := b.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по строкам, want >= 1", got)
	}
}

// TestBatcherSaturationByteCap — упор в потолок ПО БАЙТАМ при незанятом
// счётном плече: одна тяжёлая строка тяжелее потолка сама по себе не
// вычищается (trimLocked всегда оставляет хотя бы одну), поэтому байтовое
// плечо должно быть видно даже когда буфер держит одну строку из тысяч
// разрешённых по счёту.
func TestBatcherSaturationByteCap(t *testing.T) {
	b := NewBatcher(nil)
	b.maxBuf = 10000
	b.maxBufBytes = 50 // меньше веса одной строки ниже
	b.Add(Event{ID: "e", Message: strings.Repeat("m", 500)})
	if got := b.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по байтам, want >= 1", got)
	}
}

// TestBatcherSaturationPartial — частичное заполнение даёт значение строго
// между 0 и 1, а не сразу 0 или 1.
func TestBatcherSaturationPartial(t *testing.T) {
	b := NewBatcher(nil)
	b.maxBuf = 10
	b.maxBufBytes = 1 << 30
	for i := 0; i < 3; i++ {
		b.Add(Event{ID: "e", Message: "x"})
	}
	got := b.Saturation()
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
