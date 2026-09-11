package trace

import (
	"strings"
	"testing"
)

// TestSpanWriterSaturationEmpty — пустой писатель не насыщен.
func TestSpanWriterSaturationEmpty(t *testing.T) {
	w := NewSpanWriter(nil)
	if got := w.Saturation(); got != 0 {
		t.Fatalf("Saturation() = %v на пустом писателе, want 0", got)
	}
}

// TestSpanWriterSaturationTxRowCap — упор в потолок ПО СТРОКАМ транзакций при
// незанятых остальных плечах.
func TestSpanWriterSaturationTxRowCap(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 4
	w.maxSpanBuf = 1_000_000
	w.maxBufBytes = 1 << 30
	for i := 0; i < 4; i++ {
		w.Add(1, 1, sampleTx(0))
	}
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при txBuf на потолке по строкам, want >= 1", got)
	}
}

// TestSpanWriterSaturationByteCap — упор в потолок ПО БАЙТАМ при незанятых
// счётных плечах: одна тяжёлая транзакция тяжелее потолка сама по себе не
// вычищается (trim*Locked всегда оставляет хотя бы одну строку в каждом
// буфере).
func TestSpanWriterSaturationByteCap(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 10000
	w.maxSpanBuf = 100000
	w.maxBufBytes = 50 // меньше веса одной строки ниже
	tx := sampleTx(0)
	tx.Name = strings.Repeat("m", 500)
	w.Add(1, 1, tx)
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по байтам, want >= 1", got)
	}
}

// TestSpanWriterSaturationSpanShoulderSaturatesFirst — плечо spanBuf
// насыщается раньше транзакционного: транзакция с большим числом спанов
// упирается в maxSpanBuf, хотя занимает всего одну строку из txBuf. Если
// Saturation считал бы только txBuf (или усреднял плечи), находка была бы
// не видна — а именно этот случай (много спанов на одну транзакцию) и
// оправдывает раздельный учёт.
func TestSpanWriterSaturationSpanShoulderSaturatesFirst(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 1000  // транзакционный потолок далёк
	w.maxSpanBuf = 5 // спановый потолок маленький
	w.maxBufBytes = 1 << 30
	w.Add(1, 1, sampleTx(5)) // 1 транзакция, root+5 = 6 спанов > maxSpanBuf

	w.mu.Lock()
	txLen := len(w.txBuf)
	w.mu.Unlock()
	if txLen != 1 {
		t.Fatalf("txBuf = %d строк, want 1 (транзакционное плечо должно остаться почти пустым)", txLen)
	}
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при переполненном spanBuf и пустом txBuf, want >= 1", got)
	}
}

// TestSpanWriterSaturationPartial — частичное заполнение даёт значение строго
// между 0 и 1.
func TestSpanWriterSaturationPartial(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 10
	w.maxSpanBuf = 1000
	w.maxBufBytes = 1 << 30
	for i := 0; i < 3; i++ {
		w.Add(1, 1, sampleTx(0))
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
