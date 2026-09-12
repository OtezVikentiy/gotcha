package ingest

import "sync/atomic"

type IngestRejectReason string

const (
	RejectKeyUnknown IngestRejectReason = "key_unknown"
	RejectKeyRevoked IngestRejectReason = "key_revoked" // код её не производит, см. ingestRejectionPairs
	RejectKeyScope   IngestRejectReason = "key_scope"
	RejectRateLimit  IngestRejectReason = "rate_limit"
	RejectQuota      IngestRejectReason = "quota"
	RejectTooLarge   IngestRejectReason = "too_large"
	RejectMalformed  IngestRejectReason = "malformed"
	RejectOverloaded IngestRejectReason = "overloaded"
	// Отдельно от overloaded: там буфер записи, здесь — бюджет разбора профиля.
	RejectProfileDecodeBudget IngestRejectReason = "profile_decode_budget"
)

type IngestSignal string

const (
	SignalEvent       IngestSignal = "event"
	SignalTransaction IngestSignal = "transaction"
	SignalMetric      IngestSignal = "metric"
	SignalProfile     IngestSignal = "profile"
	SignalLog         IngestSignal = "log"
	SignalDeploy      IngestSignal = "deploy"
)

type IngestRejectionKey struct {
	Reason IngestRejectReason
	Signal IngestSignal
}

// Пары key_scope не выписаны литералом — вычисляются из keyScopeMatrix
// (scope.go), чтобы не держать два места, обязанных совпадать.
var ingestRejectionPairs = append([]IngestRejectionKey{
	{RejectKeyUnknown, SignalEvent}, {RejectKeyUnknown, SignalTransaction},
	{RejectKeyUnknown, SignalMetric}, {RejectKeyUnknown, SignalProfile},
	{RejectKeyUnknown, SignalLog}, {RejectKeyUnknown, SignalDeploy},

	{RejectRateLimit, SignalEvent}, {RejectRateLimit, SignalTransaction},
	{RejectRateLimit, SignalMetric}, {RejectRateLimit, SignalProfile},
	{RejectRateLimit, SignalLog}, {RejectRateLimit, SignalDeploy},

	{RejectTooLarge, SignalEvent}, {RejectTooLarge, SignalTransaction},
	{RejectTooLarge, SignalMetric}, {RejectTooLarge, SignalProfile},
	{RejectTooLarge, SignalLog}, {RejectTooLarge, SignalDeploy},

	{RejectMalformed, SignalEvent}, {RejectMalformed, SignalTransaction},
	{RejectMalformed, SignalMetric}, {RejectMalformed, SignalProfile},
	{RejectMalformed, SignalLog}, {RejectMalformed, SignalDeploy},

	{RejectQuota, SignalEvent}, {RejectQuota, SignalTransaction},
	{RejectQuota, SignalMetric}, {RejectQuota, SignalProfile},
	{RejectQuota, SignalLog},

	{RejectOverloaded, SignalEvent}, {RejectOverloaded, SignalTransaction},
	{RejectOverloaded, SignalMetric}, {RejectOverloaded, SignalProfile},
	{RejectOverloaded, SignalLog},

	// Только profile: бюджет декодирования существует лишь на pprof/sentry-путях приёма профиля.
	{RejectProfileDecodeBudget, SignalProfile},
}, keyScopeRejectionPairs()...)

// Копия — чтобы вызывающий не мог испортить общий слайс.
func IngestRejectionPairs() []IngestRejectionKey {
	return append([]IngestRejectionKey(nil), ingestRejectionPairs...)
}

func newIngestRejectCounters() map[IngestRejectionKey]*atomic.Int64 {
	m := make(map[IngestRejectionKey]*atomic.Int64, len(ingestRejectionPairs))
	for _, k := range ingestRejectionPairs {
		m[k] = new(atomic.Int64)
	}
	return m
}

func (h *Handler) countRejected(reason IngestRejectReason, signal IngestSignal) {
	if c, ok := h.rejected[IngestRejectionKey{reason, signal}]; ok {
		c.Add(1)
	}
}

func (h *Handler) RejectedBy(reason IngestRejectReason, signal IngestSignal) int64 {
	if c, ok := h.rejected[IngestRejectionKey{reason, signal}]; ok {
		return c.Load()
	}
	return 0
}

func (h *Handler) HostScopeSkipped() int64 { return h.hostScopeSkipped.Load() }
