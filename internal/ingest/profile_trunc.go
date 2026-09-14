package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Бюджет — в байтах, взвешен по размеру входа (арность душит лёгкий трафик или
// пропускает тяжёлый), привязан к доле GOMEMLIMIT — как buffers в cmd/gotcha.
const estimatedPprofAmplification = 35

type profileDecodeBudget struct {
	mu       sync.Mutex
	total    int64 // <=0 — не ограничено
	inFlight int64
}

func newProfileDecodeBudget() *profileDecodeBudget {
	return &profileDecodeBudget{}
}

func (b *profileDecodeBudget) setLimit(total int64) {
	b.mu.Lock()
	b.total = total
	b.mu.Unlock()
}

// decode'ы быстрые (миллисекунды) — короткий опрос дешевле условной переменной
// и проще проверить, что он не теряет пробуждения под гонкой.
const profileDecodeBudgetPollInterval = 2 * time.Millisecond

// acquire ждёт, пока не поместится в бюджет, или пока ctx не отменится; weight
// больше всего бюджета — отказ немедленный, ждать бессмысленно.
func (b *profileDecodeBudget) acquire(ctx context.Context, weight int64) bool {
	b.mu.Lock()
	total := b.total
	b.mu.Unlock()
	if total <= 0 {
		return true
	}
	if weight > total {
		return false
	}
	for {
		b.mu.Lock()
		if b.inFlight+weight <= b.total {
			b.inFlight += weight
			b.mu.Unlock()
			return true
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(profileDecodeBudgetPollInterval):
		}
	}
}

func (b *profileDecodeBudget) release(weight int64) {
	b.mu.Lock()
	if b.total > 0 {
		b.inFlight -= weight
	}
	b.mu.Unlock()
}

// saturation — только для лога при отказе (см. profileDecodeBudgetExhausted),
// в самом acquire не участвует.
func (b *profileDecodeBudget) saturation() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total <= 0 {
		return 0
	}
	return float64(b.inFlight) / float64(b.total)
}

// Долю от GOMEMLIMIT считает вызывающий (cmd/gotcha) — ingest про cgroup не знает.
func (h *Handler) SetProfileDecodeBudgetBytes(n int64) {
	h.profileDecodeBudget.setLimit(n)
}

// rawBytes — размер входа ПОСЛЕ gunzip, как у pp.ParseData. release() — сразу
// после декодирования, не после всей остальной обработки запроса.
func (h *Handler) acquireProfileDecode(ctx context.Context, rawBytes int) (release func(), ok bool) {
	weight := int64(rawBytes) * estimatedPprofAmplification
	if !h.profileDecodeBudget.acquire(ctx, weight) {
		return func() {}, false
	}
	return func() { h.profileDecodeBudget.release(weight) }, true
}

// Не переиспользует overloaded()/RejectOverloaded: та причина — просевший буфер
// записи, эта — свой бюджет разбора; оператору нужно различать их.
func (h *Handler) profileDecodeBudgetExhausted(w http.ResponseWriter, orgID, projectID int64) {
	slog.Warn("ingest: pprof profile decode budget exhausted",
		"project_id", projectID, "org_id", orgID, "saturation", h.profileDecodeBudget.saturation())
	h.countRejected(RejectProfileDecodeBudget, SignalProfile)
	w.Header().Set("Retry-After", "5")
	writeJSONError(w, http.StatusServiceUnavailable, "ingest overloaded")
}

// Отдельно от rejected_total: профиль здесь принят (200/202), просто часть
// сэмплов/кадров срезана капом decode-бюджета — другой класс события.
type ProfileParser string

const (
	ParserPprof  ProfileParser = "pprof"
	ParserSentry ProfileParser = "sentry"
)

func ProfileParsers() []ProfileParser {
	return []ProfileParser{ParserPprof, ParserSentry}
}

func newProfileTruncatedCounters() map[ProfileParser]*atomic.Int64 {
	return map[ProfileParser]*atomic.Int64{
		ParserPprof:  new(atomic.Int64),
		ParserSentry: new(atomic.Int64),
	}
}

func (h *Handler) countProfileTruncated(parser ProfileParser) {
	if c, ok := h.profileTruncated[parser]; ok {
		c.Add(1)
	}
}

func (h *Handler) ProfileTruncatedBy(parser ProfileParser) int64 {
	if c, ok := h.profileTruncated[parser]; ok {
		return c.Load()
	}
	return 0
}
