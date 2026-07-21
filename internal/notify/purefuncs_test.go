package notify

import (
	"errors"
	"strings"
	"testing"
)

// TestErrString — nil-ошибка даёт пустую строку.
func TestErrString(t *testing.T) {
	if errString(nil) != "" {
		t.Error("nil → пусто")
	}
	if errString(errors.New("boom")) != "boom" {
		t.Error("ошибка → её текст")
	}
}

// TestTelegramBaseURL — дефолтный хост API, если BaseURL не задан.
func TestTelegramBaseURL(t *testing.T) {
	if (&TelegramSender{}).baseURL() != defaultTelegramBaseURL {
		t.Error("пустой BaseURL → дефолт")
	}
	if (&TelegramSender{BaseURL: "https://tg.local"}).baseURL() != "https://tg.local" {
		t.Error("явный BaseURL должен сохраняться")
	}
}

// TestRedactToken — токен вырезается из строки; пустой токен не меняет строку.
func TestRedactToken(t *testing.T) {
	if got := redactToken("url/bot123:secret/x", ""); got != "url/bot123:secret/x" {
		t.Errorf("пустой токен не должен ничего менять: %q", got)
	}
	got := redactToken("GET https://api/bot987:AAA/send", "987:AAA")
	if strings.Contains(got, "987:AAA") || !strings.Contains(got, "<redacted>") {
		t.Errorf("токен не вырезан: %q", got)
	}
}
