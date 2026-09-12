package i18n

import (
	"hash/maphash"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type MissingKeyStage string

const (
	// Ключа нет в запрошенной локали, но нашёлся в дефолтной — страница молча показывает чужой язык.
	MissingKeyFallback MissingKeyStage = "fallback"
	// Ключа нет нигде — страница показывает сырой идентификатор ключа как есть.
	MissingKeyMissing MissingKeyStage = "missing"
)

// Дедуп только лога — счётчик метрики считает КАЖДЫЙ промах независимо от дедупликации.
const missingKeyLogInterval = time.Minute

// key — из пользовательских данных, карта на него росла бы неограниченно; кольцо держит память константной.
// Коллизия слота изредка гасит один лог раньше срока — счётчик метрики от этого не страдает вовсе.
const missingKeyLogGateSlots = 4096

var (
	// Ключ карты — locale+"\x00"+stage, значения — *atomic.Int64.
	missingKeyCounts sync.Map
	// Один на процесс — нужна лишь равномерность по слотам, не непредсказуемость между запусками.
	missingKeyLogGateSeed = maphash.MakeSeed()
	// Индекс — hash(locale,stage,key) % Slots; значение — время последнего лога, наносекунды Unix.
	missingKeyLogGate [missingKeyLogGateSlots]atomic.Int64
)

func missingKeyLogGateIndex(locale, key string, stage MissingKeyStage) int {
	var h maphash.Hash
	h.SetSeed(missingKeyLogGateSeed)
	_, _ = h.WriteString(locale)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(string(stage))
	_ = h.WriteByte(0)
	_, _ = h.WriteString(key)
	return int(h.Sum64() % missingKeyLogGateSlots)
}

// Вызывается из горячего пути рендера, возможно параллельно — обе структуры данных lock-free.
func recordMissingKey(locale, key string, stage MissingKeyStage) {
	counterKey := locale + "\x00" + string(stage)
	c, _ := missingKeyCounts.LoadOrStore(counterKey, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)

	gate := &missingKeyLogGate[missingKeyLogGateIndex(locale, key, stage)]
	now := time.Now().UnixNano()
	last := gate.Load()
	if now-last < int64(missingKeyLogInterval) {
		return
	}
	if !gate.CompareAndSwap(last, now) {
		// Проиграли гонку другой горутине — она либо уже залогировала, либо логирует сейчас.
		return
	}
	slog.Warn("i18n: перевод не найден", "key", key, "locale", locale, "stage", string(stage))
}

// Существует для регистрации self-метрики по каждой локали без ручного перечисления в cmd/gotcha.
func SupportedLocales() []string {
	out := make([]string, 0, len(catalogs))
	for code := range catalogs {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

func MissingKeyStages() []MissingKeyStage {
	return []MissingKeyStage{MissingKeyFallback, MissingKeyMissing}
}

// Потокобезопасно и дёшево — self-метрики читают его при каждом снятии показаний без блокировок.
func MissingKeyTotal(locale string, stage MissingKeyStage) int64 {
	v, ok := missingKeyCounts.Load(locale + "\x00" + string(stage))
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}
