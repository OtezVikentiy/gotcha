package i18n

import (
	"context"
	"testing"
)

func TestPluralLookupFormFallbackToOther(t *testing.T) {
	got := pluralLookup("en", "chart.bar.transactions", "few")
	if got != "{n} transactions" {
		t.Fatalf("pluralLookup(en, ..., few) = %q, want other-форму %q", got, "{n} transactions")
	}
}

func TestPluralLookupMissingKeyReturnsKey(t *testing.T) {
	if got := pluralLookup("en", "no.such.plural", "one"); got != "no.such.plural" {
		t.Fatalf("pluralLookup отсутствующего ключа = %q, want сам ключ", got)
	}
}

func TestPluralLookupUnknownLocaleFallsToDefault(t *testing.T) {
	got := pluralLookup("fr", "issue.times_seen", "one")
	if got != "{n} раз" {
		t.Fatalf("pluralLookup(fr, ...) = %q, want ru-форму %q", got, "{n} раз")
	}
}

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

func TestTnMissingKey(t *testing.T) {
	ru := WithLocale(context.Background(), Locale{Code: "ru"})
	if got := Tn(ru, "no.such.plural", 3); got != "no.such.plural" {
		t.Fatalf("Tn отсутствующего ключа = %q", got)
	}
}

func TestLookupDefaultLocaleMissingKey(t *testing.T) {
	if got := lookup("ru", "no.such.message.key"); got != "no.such.message.key" {
		t.Fatalf("lookup(ru, missing) = %q, want сам ключ", got)
	}
}

func TestPluralFormNegative(t *testing.T) {
	if got := pluralForm("ru", -1); got != "one" {
		t.Fatalf("pluralForm(ru, -1) = %q, want one", got)
	}
	if got := pluralForm("en", -1); got != "one" {
		t.Fatalf("pluralForm(en, -1) = %q, want one", got)
	}
}
