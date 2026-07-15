package trace

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// Keep решает, оставлять ли трейс при заданной доле семплирования rate.
// Решение ДЕТЕРМИНИРОВАННОЕ по trace_id (хеш, а не math/rand): спаны одного
// трейса могут приехать на разные реплики, и все они должны принять одно и то
// же решение — половина трейса хуже отсутствующего.
//
// Канонический вид trace_id — hex в НИЖНЕМ регистре; Keep приводит вход к нему
// сам (обрезка пробелов + ToLower) и только потом хеширует. Иначе "4BF9…" и
// "4bf9…" — разные хеши и разные решения, а регистр выбирает тот, кто кодирует
// id: в OTLP он едет 16 сырыми байтами, и hex из него делает уже приёмник.
// Ingest хранит id в том же каноническом виде (см. ingest.normalizeID), чтобы
// не разъехались ни семплирование, ни join spans↔transactions.
//
// rate <= 0 (и NaN) → false, rate >= 1 → true.
func Keep(traceID string, rate float64) bool {
	if !(rate > 0) { // NaN тоже сюда
		return false
	}
	if rate >= 1 {
		return true
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(traceID))))
	h := binary.BigEndian.Uint64(sum[:8])
	// Старшие 53 бита → равномерное [0,1) без потерь точности float64.
	return float64(h>>11)/float64(uint64(1)<<53) < rate
}
