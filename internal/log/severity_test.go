package log

import "testing"

func TestCanonFromNumber(t *testing.T) {
	cases := map[int32]string{
		0: SevInfo, 1: SevTrace, 4: SevTrace, 5: SevDebug, 8: SevDebug,
		9: SevInfo, 12: SevInfo, 13: SevWarn, 16: SevWarn,
		17: SevError, 20: SevError, 21: SevFatal, 24: SevFatal, 99: SevInfo,
	}
	for n, want := range cases {
		if got := CanonFromNumber(n); got != want {
			t.Errorf("CanonFromNumber(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestCanonFromText(t *testing.T) {
	cases := map[string]string{
		"ERROR": SevError, "error": SevError, "err": SevError, "critical": SevFatal,
		"warn": SevWarn, "warning": SevWarn, "info": SevInfo, "debug": SevDebug,
		"trace": SevTrace, "fatal": SevFatal, "17": SevError, "": SevInfo, "zzz": SevInfo,
		// Переполнение int32 — не SeverityNumber: ParseInt(,,32) отвергает, падаем в словарь → SevInfo.
		"4294967297": SevInfo, "2147483648": SevInfo,
	}
	for in, want := range cases {
		if got := CanonFromText(in); got != want {
			t.Errorf("CanonFromText(%q)=%q, want %q", in, got, want)
		}
	}
}
