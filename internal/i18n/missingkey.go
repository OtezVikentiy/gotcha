package i18n

import (
	"hash/maphash"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MissingKeyStage — на каком этапе разрешения ключа перевода случился
// промах. Различает две принципиально разные ситуации для оператора:
// "показали чужой язык" и "показали сырой идентификатор ключа".
type MissingKeyStage string

const (
	// MissingKeyFallback — ключа нет в запрошенной локали, но нашёлся в
	// локали по умолчанию: страница молча показывает чужой язык.
	MissingKeyFallback MissingKeyStage = "fallback"
	// MissingKeyMissing — ключа нет ни в запрошенной локали, ни в локали по
	// умолчанию: страница показывает сырой идентификатор ключа как есть.
	MissingKeyMissing MissingKeyStage = "missing"
)

// missingKeyLogInterval — минимальный интервал между двумя предупреждениями
// в лог про один и тот же промах (locale, stage, key). Счётчик метрики
// считает КАЖДЫЙ промах независимо от того, попал он в лог или был подавлен
// дедупликацией.
const missingKeyLogInterval = time.Minute

// missingKeyLogGateSlots — число слотов кольца дедупликации лога промахов.
// Ключ дедупликации — тройка (locale, stage, key), а key приходит из
// пользовательских данных (см. Params.Status/Params.Level в
// internal/web/exports.go): оператор проекта может создать заявку с
// произвольным значением, которое затем рендерится через i18n.T как ключ
// перевода. Карта с таким ключом росла бы неограниченно на каждое уникальное
// значение за время жизни процесса. Фиксированный массив держит память
// константной независимо от числа уникальных ключей: коллизия слота лишь
// изредка подавляет один warn-лог раньше срока — для дедупликации логов это
// приемлемая цена. Счётчик метрики (missingKeyCounts, ограничен парой
// locale/stage) от коллизий не страдает вовсе — ведётся отдельно и считает
// ВСЕ промахи без исключений.
const missingKeyLogGateSlots = 4096

var (
	// missingKeyCounts — счётчик промахов по паре (locale, stage). Значения —
	// *atomic.Int64, ключ — locale+"\x00"+stage.
	missingKeyCounts sync.Map
	// missingKeyLogGateSeed — сид хэширования тройки (locale, stage, key) в
	// индекс слота missingKeyLogGate. Один на процесс: нужна лишь
	// равномерность распределения по слотам, не непредсказуемость между
	// запусками.
	missingKeyLogGateSeed = maphash.MakeSeed()
	// missingKeyLogGate — время последнего лога по слоту, в наносекундах
	// Unix. Индекс слота — hash(locale, stage, key) % missingKeyLogGateSlots.
	missingKeyLogGate [missingKeyLogGateSlots]atomic.Int64
)

// missingKeyLogGateIndex — индекс слота missingKeyLogGate для тройки
// (locale, stage, key).
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

// recordMissingKey — учитывает один промах поиска ключа перевода: без
// исключений инкрементирует счётчик по (locale, stage) и не чаще раза в
// minInterval на одну и ту же тройку (locale, stage, key) пишет
// предупреждение в лог. Вызывается из горячего пути рендера, возможно
// параллельно из многих горутин — обе структуры данных lock-free.
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
		// Проиграли гонку другой горутине — она либо только что залогировала
		// этот же промах, либо сама сейчас пытается. В обоих случаях повторный
		// лог за это окно не нужен.
		return
	}
	slog.Warn("i18n: перевод не найден", "key", key, "locale", locale, "stage", string(stage))
}

// SupportedLocales — коды загруженных каталогов перевода в стабильном
// (отсортированном) порядке. Существует для регистрации self-метрики
// gotcha_i18n_missing_key_total по каждой паре (locale, stage) без ручного
// перечисления кодов локалей в cmd/gotcha.
func SupportedLocales() []string {
	out := make([]string, 0, len(catalogs))
	for code := range catalogs {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// MissingKeyStages — обе стадии промаха поиска ключа перевода, в стабильном
// порядке (fallback, затем missing).
func MissingKeyStages() []MissingKeyStage {
	return []MissingKeyStage{MissingKeyFallback, MissingKeyMissing}
}

// MissingKeyTotal — снимок счётчика промахов lookup/pluralLookup для пары
// (locale, stage) с начала процесса. Потокобезопасно и дёшево: вызывающая
// сторона (self-метрики) читает его как func() int64 при каждом снятии
// показаний, без блокировок на горячем пути рендера.
func MissingKeyTotal(locale string, stage MissingKeyStage) int64 {
	v, ok := missingKeyCounts.Load(locale + "\x00" + string(stage))
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}
