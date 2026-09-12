package trace

import (
	"strings"
	"testing"
)

func TestSpanWriterSaturationEmpty(t *testing.T) {
	w := NewSpanWriter(nil)
	if got := w.Saturation(); got != 0 {
		t.Fatalf("Saturation() = %v на пустом писателе, want 0", got)
	}
}

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

func TestSpanWriterSaturationByteCap(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 10000
	w.maxSpanBuf = 100000
	w.maxBufBytes = 50
	tx := sampleTx(0)
	tx.Name = strings.Repeat("m", 500)
	w.Add(1, 1, tx)
	if got := w.Saturation(); got < 1 {
		t.Fatalf("Saturation() = %v при буфере на потолке по байтам, want >= 1", got)
	}
}

func TestSpanWriterSaturationSpanShoulderSaturatesFirst(t *testing.T) {
	w := NewSpanWriter(nil)
	w.maxBuf = 1000
	w.maxSpanBuf = 5
	w.maxBufBytes = 1 << 30
	w.Add(1, 1, sampleTx(5))

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

func TestBufSaturationZeroDenominatorIsUnbounded(t *testing.T) {
	if got := bufSaturation(5, 0); got != 0 {
		t.Fatalf("bufSaturation(5, 0) = %v, want 0", got)
	}
	if got := bufSaturation(5, -1); got != 0 {
		t.Fatalf("bufSaturation(5, -1) = %v, want 0", got)
	}
}
