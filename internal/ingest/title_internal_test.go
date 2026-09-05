package ingest

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
)

// TestTitleFallbacks — порядок фолбэков заголовка: exception → message →
// transaction → logger. Если пусто везде, заголовок ОСТАЁТСЯ пустым:
// подделывать его константой в данных нельзя, заглушку рисует интерфейс.
func TestTitleFallbacks(t *testing.T) {
	cases := []struct {
		name string
		pe   ParsedEvent
		want string
	}{
		{"exception", ParsedEvent{Exceptions: []fingerprint.Exception{{Type: "ValueError", Value: "boom"}}, Message: "msg", Transaction: "GET /x", Logger: "app"}, "ValueError: boom"},
		{"exception без type/value → message", ParsedEvent{Exceptions: []fingerprint.Exception{{}}, Message: "msg\nвторая строка", Transaction: "GET /x"}, "msg"},
		{"message", ParsedEvent{Message: "first\nsecond", Transaction: "GET /x", Logger: "app"}, "first"},
		{"только transaction", ParsedEvent{Transaction: "GET /orders"}, "GET /orders"},
		{"transaction перед logger", ParsedEvent{Transaction: "GET /orders", Logger: "app.worker"}, "GET /orders"},
		{"только logger", ParsedEvent{Logger: "app.worker"}, "app.worker"},
		{"вообще ничего", ParsedEvent{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := tc.pe
			got, _ := titleAndCulprit(&pe)
			if got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseEventTitleFromTransactionAndLogger — верхнеуровневые transaction и
// logger реально доезжают из JSON события до заголовка (и каппятся, как
// прочие недоверенные строки).
func TestParseEventTitleFromTransactionAndLogger(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"transaction", `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error","transaction":"GET /orders","logger":"app.worker"}`, "GET /orders"},
		{"logger", `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error","logger":"app.worker"}`, "app.worker"},
		{"ничего", `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe, err := ParseEvent([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if pe.Title != tc.want {
				t.Fatalf("title = %q, want %q", pe.Title, tc.want)
			}
		})
	}

	long := make([]byte, 0, 400)
	for i := 0; i < 300; i++ {
		long = append(long, 'x')
	}
	pe, err := ParseEvent([]byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","transaction":"` + string(long) + `"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len([]rune(pe.Transaction)); got != 200 {
		t.Fatalf("transaction должен каппиться до 200 рун: %d", got)
	}
	if got := len([]rune(pe.Title)); got != 200 {
		t.Fatalf("заголовок из transaction должен каппиться до 200 рун: %d", got)
	}
}
