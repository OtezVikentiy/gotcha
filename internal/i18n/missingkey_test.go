package i18n

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// TestLookupFallbackStageLogsAndCounts — ключ отсутствует в выдуманной
// локали, но есть в Messages локали по умолчанию: lookup обязан вернуть
// значение из дефолтной локали и учесть это как MissingKeyFallback.
func TestLookupFallbackStageLogsAndCounts(t *testing.T) {
	var key string
	for k := range catalogs[Default.Code].Messages {
		key = k
		break
	}
	if key == "" {
		t.Skip("в дефолтном каталоге нет ключей messages")
	}
	const fakeCode = "zz-fallback-locale-catalog"
	want := catalogs[Default.Code].Messages[key]
	before := MissingKeyTotal(fakeCode, MissingKeyFallback)

	got := lookup(fakeCode, key)

	if got != want {
		t.Fatalf("lookup(%q,%q) = %q, want fallback-значение из дефолтной локали %q", fakeCode, key, got, want)
	}
	if after := MissingKeyTotal(fakeCode, MissingKeyFallback); after-before != 1 {
		t.Fatalf("MissingKeyTotal(%q, fallback) delta = %d, want 1", fakeCode, after-before)
	}
}

// TestLookupFullMissReturnsKeyAndCounts — ключа нет нигде: рендер не падает,
// возвращает сам ключ, промах учитывается как MissingKeyMissing.
func TestLookupFullMissReturnsKeyAndCounts(t *testing.T) {
	const fakeKey = "zz.totally.made.up.missing.key.t5.catalog"
	before := MissingKeyTotal(Default.Code, MissingKeyMissing)

	got := lookup(Default.Code, fakeKey)

	if got != fakeKey {
		t.Fatalf("lookup(default,%q) = %q, want сырой ключ обратно", fakeKey, got)
	}
	if after := MissingKeyTotal(Default.Code, MissingKeyMissing); after-before != 1 {
		t.Fatalf("MissingKeyTotal(default, missing) delta = %d, want 1", after-before)
	}
}

// TestTOnPluralOnlyKeyIsMissing — ключ, живущий только в секции "plurals",
// вызванный через T(): lookup смотрит только в Messages, поэтому это тот же
// класс промаха ("missing"), не особый случай.
func TestTOnPluralOnlyKeyIsMissing(t *testing.T) {
	var pluralOnlyKey string
	for k := range catalogs[Default.Code].Plurals {
		if _, inMessages := catalogs[Default.Code].Messages[k]; !inMessages {
			pluralOnlyKey = k
			break
		}
	}
	if pluralOnlyKey == "" {
		t.Skip("нет ключей, присутствующих только в plurals — тест неприменим к текущим каталогам")
	}
	ctx := WithLocale(context.Background(), Locale{Code: Default.Code})
	before := MissingKeyTotal(Default.Code, MissingKeyMissing)

	got := T(ctx, pluralOnlyKey)

	if got != pluralOnlyKey {
		t.Fatalf("T(ctx,%q) = %q, want сырой ключ обратно (T не видит plurals)", pluralOnlyKey, got)
	}
	if after := MissingKeyTotal(Default.Code, MissingKeyMissing); after-before != 1 {
		t.Fatalf("MissingKeyTotal(default, missing) delta = %d, want 1", after-before)
	}
}

// TestPluralLookupFallbackStageLogsAndCounts — то же, что
// TestLookupFallbackStageLogsAndCounts, но для pluralLookup/Plurals.
func TestPluralLookupFallbackStageLogsAndCounts(t *testing.T) {
	var key string
	for k := range catalogs[Default.Code].Plurals {
		key = k
		break
	}
	if key == "" {
		t.Skip("в дефолтном каталоге нет plural-ключей")
	}
	form := pluralForm(Default.Code, 5) // форма, гарантированная TestPluralFormsComplete
	want := pluralLookup(Default.Code, key, form)
	const fakeCode = "zz-fallback-locale-plural"
	before := MissingKeyTotal(fakeCode, MissingKeyFallback)

	got := pluralLookup(fakeCode, key, form)

	if got != want {
		t.Fatalf("pluralLookup(%q,%q,%q) = %q, want fallback-значение %q", fakeCode, key, form, got, want)
	}
	if after := MissingKeyTotal(fakeCode, MissingKeyFallback); after-before != 1 {
		t.Fatalf("MissingKeyTotal(%q, fallback) delta = %d, want 1", fakeCode, after-before)
	}
}

// TestPluralLookupFullMissReturnsKeyAndCounts — plural-ключа нет нигде.
func TestPluralLookupFullMissReturnsKeyAndCounts(t *testing.T) {
	const fakeKey = "zz.totally.made.up.missing.plural.key.t5"
	before := MissingKeyTotal(Default.Code, MissingKeyMissing)

	got := pluralLookup(Default.Code, fakeKey, "other")

	if got != fakeKey {
		t.Fatalf("pluralLookup(default,%q,other) = %q, want сырой ключ обратно", fakeKey, got)
	}
	if after := MissingKeyTotal(Default.Code, MissingKeyMissing); after-before != 1 {
		t.Fatalf("MissingKeyTotal(default, missing) delta = %d, want 1", after-before)
	}
}

// TestMissingKeyLogDedupDoesNotEatCounter — дедупликация лога (раз в минуту
// на одну и ту же тройку locale/stage/key) не должна занижать счётчик:
// метрика обязана посчитать оба промаха, а лог — подавить второй.
//
// missingKeyLogGate — фиксированный набор слотов на весь процесс: он не
// сбрасывается ни между итерациями `go test -count=N` в одном бинарнике, ни
// между тестами пакета, а слот, на который хэшируется тройка
// (locale, stage, key), может быть недавно выставлен ЛЮБЫМ другим ключом,
// упавшим в тот же слот (сам гейт по конструкции экономит память ценой
// редких коллизий между разными ключами — см. missingkey.go). Тест обнуляет
// свой слот явно перед прогоном, поэтому не зависит ни от глобального
// состояния, ни от порядка запуска, ни от того, что до него делали другие
// тесты пакета (включая TestMissingKeyLogGateMemoryIsConstant, который
// намеренно засевает почти все слоты).
func TestMissingKeyLogDedupDoesNotEatCounter(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const fakeCode = "zz-dedup-locale"
	const fakeKey = "zz.dedup.missing.key.t5"
	missingKeyLogGate[missingKeyLogGateIndex(fakeCode, fakeKey, MissingKeyMissing)].Store(0)
	before := MissingKeyTotal(fakeCode, MissingKeyMissing)

	lookup(fakeCode, fakeKey)
	lookup(fakeCode, fakeKey)

	if after := MissingKeyTotal(fakeCode, MissingKeyMissing); after-before != 2 {
		t.Fatalf("MissingKeyTotal delta = %d, want 2 (счётчик обязан считать оба промаха)", after-before)
	}
	if n := strings.Count(buf.String(), fakeKey); n != 1 {
		t.Fatalf("лог содержит key %d раз(а) за минуту, want 1 (дедупликация не сработала): %s", n, buf.String())
	}
}

// TestSupportedLocalesAndStages — контракт снимка для self-метрик (T6):
// стабильный, отсортированный список локалей и фиксированный порядок стадий.
func TestSupportedLocalesAndStages(t *testing.T) {
	locales := SupportedLocales()
	if len(locales) != 2 || locales[0] != "en" || locales[1] != "ru" {
		t.Fatalf("SupportedLocales() = %v, want [en ru]", locales)
	}
	stages := MissingKeyStages()
	if len(stages) != 2 || stages[0] != MissingKeyFallback || stages[1] != MissingKeyMissing {
		t.Fatalf("MissingKeyStages() = %v, want [fallback missing]", stages)
	}
}

// TestMissingKeyLogGateMemoryIsConstant — гейт дедупликации лога
// (missingKeyLogGate) обязан быть структурой с константным объёмом памяти,
// а не картой без предела: key приходит из пользовательских данных
// (Params.Status/Params.Level в internal/web/exports.go рендерятся как
// ключи перевода), и оператор проекта может навсегда осадить в гейте одну
// запись на каждое уникальное значение. Проверяем это наблюдаемым свойством
// — типом и числом слотов — а не замером heap, который флакует: сначала
// убеждаемся, что missingKeyLogGate вообще является массивом фиксированного
// размера (не map), затем прогоняем 100000 заведомо уникальных промахов и
// перепроверяем, что число слотов не изменилось. reflect.TypeOf берёт тип
// через указатель на глобал (&missingKeyLogGate), а не по значению — иначе
// go vet справедливо ругается на копирование atomic.Int64 (copylocks).
func TestMissingKeyLogGateMemoryIsConstant(t *testing.T) {
	gateType := reflect.TypeOf(&missingKeyLogGate).Elem()
	if gateType.Kind() != reflect.Array {
		t.Fatalf("missingKeyLogGate должен быть массивом фиксированного размера, а не %s — иначе гейт снова течёт по числу уникальных ключей", gateType.Kind())
	}
	before := gateType.Len()
	if before != missingKeyLogGateSlots {
		t.Fatalf("missingKeyLogGate имеет %d слотов, want missingKeyLogGateSlots = %d", before, missingKeyLogGateSlots)
	}

	const fakeCode = "zz-mem-const-locale"
	for i := 0; i < 100000; i++ {
		lookup(fakeCode, fmt.Sprintf("zz.mem.const.missing.key.%d", i))
	}

	after := reflect.TypeOf(&missingKeyLogGate).Elem().Len()
	if after != before {
		t.Fatalf("число слотов missingKeyLogGate изменилось с %d на %d после 100000 уникальных промахов — гейт течёт", before, after)
	}
}
