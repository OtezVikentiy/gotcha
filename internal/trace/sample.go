package trace

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// решение детерминировано по trace_id (хеш, не math/rand) — спаны трейса на разных репликах
// должны сойтись на одном; канонизация (lower+trim) обязана совпадать с ingest.normalizeID.
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
