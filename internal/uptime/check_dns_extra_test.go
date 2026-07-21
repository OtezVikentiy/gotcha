package uptime

import (
	"context"
	"fmt"
	"net"
	"testing"
)

// TestAnswerMatches — чистая функция сопоставления ответов DNS: MX сравнивается
// целиком регистронезависимо, TXT — подстрокой, остальные типы — точным
// совпадением. Покрывает все ветки switch, включая пустой набор ответов.
func TestAnswerMatches(t *testing.T) {
	cases := []struct {
		name       string
		recordType string
		answers    []string
		expected   string
		want       bool
	}{
		{"MX case-insensitive match", "MX", []string{"MAIL.example.com"}, "mail.example.com", true},
		{"MX no match", "MX", []string{"mail.example.com"}, "other.example.com", false},
		{"TXT substring match", "TXT", []string{"v=spf1 include:_spf.example.com ~all"}, "include:_spf.example.com", true},
		{"TXT no match", "TXT", []string{"v=spf1 ~all"}, "verification=abc", false},
		{"A exact match", "A", []string{"1.2.3.4", "5.6.7.8"}, "5.6.7.8", true},
		{"A exact no match", "A", []string{"1.2.3.4"}, "9.9.9.9", false},
		{"CNAME exact match", "CNAME", []string{"target.example.com"}, "target.example.com", true},
		{"empty answers", "A", nil, "1.2.3.4", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := answerMatches(c.recordType, c.answers, c.expected); got != c.want {
				t.Fatalf("answerMatches(%q,%v,%q) = %v, want %v", c.recordType, c.answers, c.expected, got, c.want)
			}
		})
	}
}

// TestLookupUnsupportedRecordType: неизвестный тип записи → ошибка сразу, без
// обращения к сети (ветка default в switch).
func TestLookupUnsupportedRecordType(t *testing.T) {
	if _, err := lookup(context.Background(), net.DefaultResolver, "SRV", "example.com"); err == nil {
		t.Fatal("lookup(SRV) = nil error, want unsupported-type error")
	}
}

// TestLookupResolverErrors: резолвер, у которого Dial всегда падает, гарантирует,
// что каждая ветка типа записи (A/AAAA/CNAME/MX/TXT) прогоняет свой путь
// возврата ошибки, детерминированно и без реальной сети.
func TestLookupResolverErrors(t *testing.T) {
	failing := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, fmt.Errorf("dns unavailable")
		},
	}
	for _, rt := range []string{"A", "AAAA", "CNAME", "MX", "TXT"} {
		if _, err := lookup(context.Background(), failing, rt, "example.com"); err == nil {
			t.Errorf("lookup(%s) with failing resolver = nil error, want error", rt)
		}
	}
}
