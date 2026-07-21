package i18n

import (
	"context"
	"testing"
)

// TestPluralLookupFormFallbackToOther — форма, которой нет в каталоге, схлопывается
// в "other". Английский chart.bar.transactions объявляет только one/other, поэтому
// запрос формы "few" (её выдаёт русский pluralForm, но не английский) должен дать
// строку из "other", а не сам ключ.
func TestPluralLookupFormFallbackToOther(t *testing.T) {
	got := pluralLookup("en", "chart.bar.transactions", "few")
	if got != "{n} transactions" {
		t.Fatalf("pluralLookup(en, ..., few) = %q, want other-форму %q", got, "{n} transactions")
	}
}

// TestPluralLookupKeyFallbackToDefault — отсутствующий у запрошенной локали ключ
// добирается из дефолтной локали (ru). У en нет ключа "no.such.plural", у ru тоже
// нет → в итоге возвращается сам ключ (видимый маркер отсутствия перевода).
func TestPluralLookupMissingKeyReturnsKey(t *testing.T) {
	if got := pluralLookup("en", "no.such.plural", "one"); got != "no.such.plural" {
		t.Fatalf("pluralLookup отсутствующего ключа = %q, want сам ключ", got)
	}
}

// TestPluralLookupUnknownLocaleFallsToDefault — неизвестный код локали не имеет
// каталога вовсе; lookup обязан уйти в дефолтную (ru) и вернуть её текст.
func TestPluralLookupUnknownLocaleFallsToDefault(t *testing.T) {
	got := pluralLookup("fr", "issue.times_seen", "one")
	if got != "{n} раз" {
		t.Fatalf("pluralLookup(fr, ...) = %q, want ru-форму %q", got, "{n} раз")
	}
}

// TestTnRuFewMany — Tn по-русски проходит все три категории (one/few/many) с
// подстановкой {n}. Дополняет существующий TestTn граничными числами 22 и 111.
func TestTnRuBoundaries(t *testing.T) {
	ru := WithLocale(context.Background(), Locale{Code: "ru"})
	cases := map[int]string{
		22:  "22 раза", // mod10==2, mod100==22 → few
		111: "111 раз", // mod100==11 → many, не one
	}
	for n, want := range cases {
		if got := Tn(ru, "issue.times_seen", n); got != want {
			t.Fatalf("Tn(ru, issue.times_seen, %d) = %q, want %q", n, got, want)
		}
	}
}

// TestTnMissingKey — Tn на отсутствующем ключе возвращает ключ с подставленным {n}
// (здесь плейсхолдера в ключе нет, поэтому просто сам ключ).
func TestTnMissingKey(t *testing.T) {
	ru := WithLocale(context.Background(), Locale{Code: "ru"})
	if got := Tn(ru, "no.such.plural", 3); got != "no.such.plural" {
		t.Fatalf("Tn отсутствующего ключа = %q", got)
	}
}

// TestLookupDefaultLocaleMissingKey — прямое покрытие ветки lookup, где локаль уже
// дефолтная (ru) и ключа нет: блок дополнительного fallback пропускается и сразу
// возвращается сам ключ.
func TestLookupDefaultLocaleMissingKey(t *testing.T) {
	if got := lookup("ru", "no.such.message.key"); got != "no.such.message.key" {
		t.Fatalf("lookup(ru, missing) = %q, want сам ключ", got)
	}
}

// TestPluralFormNegative — отрицательное n нормализуется по модулю: -1 → one.
func TestPluralFormNegative(t *testing.T) {
	if got := pluralForm("ru", -1); got != "one" {
		t.Fatalf("pluralForm(ru, -1) = %q, want one", got)
	}
	if got := pluralForm("en", -1); got != "one" {
		t.Fatalf("pluralForm(en, -1) = %q, want one", got)
	}
}
