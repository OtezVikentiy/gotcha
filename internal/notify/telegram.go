package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"gitflic.ru/otezvikentiy/gotcha/internal/netguard"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

// TelegramSender шлёт уведомление через Telegram Bot API. Пустой BaseURL —
// api.telegram.org; непустой задаёт оператор инстанса
// (GOTCHA_TELEGRAM_API_BASE) — свой telegram-bot-api или прокси, когда до
// api.telegram.org не достучаться. Тесты подставляют сюда httptest.
//
// it-sec P2-1 (2026-08-12): BaseURL сегодня задаёт только оператор, не
// арендатор (bot-токен идёт в path, @ туда host не подменяет), поэтому это
// не активная SSRF-дыра. Тем не менее исходящий запрос проведён через тот
// же netguard-транспорт, что и WebhookSender — для единообразия
// defense-in-depth и на случай, если base URL когда-нибудь станет
// пер-канальным (арендаторским). AllowPrivate — тот же экейпхэтч, что и у
// webhook (см. Config.SSRFAllowPrivateTelegram): нужен операторам, у
// которых свой telegram-bot-api/прокси на приватном адресе.
type TelegramSender struct {
	Client       *http.Client
	BaseURL      string
	AllowPrivate bool

	safeOnce   sync.Once
	safeClient *http.Client
}

func (s *TelegramSender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	s.safeOnce.Do(func() {
		s.safeClient = netguard.SafeHTTPClient(s.AllowPrivate, httpClientTimeout)
	})
	return s.safeClient
}

func (s *TelegramSender) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return defaultTelegramBaseURL
}

type telegramSendMessage struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// Send постит сообщение через {BaseURL}/bot{t.Secret}/sendMessage. t.Target
// — chat_id, payload["body"] — текст сообщения.
func (s *TelegramSender) Send(ctx context.Context, t Target, payload map[string]any) error {
	text, _ := payload["body"].(string)
	body, err := json.Marshal(telegramSendMessage{
		ChatID:                t.Target,
		Text:                  text,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("notify: telegram marshal: %w", err)
	}

	reqURL := fmt.Sprintf("%s/bot%s/sendMessage", s.baseURL(), t.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		// *url.Error embeds the full request URL — including the bot
		// token from the /bot{secret}/sendMessage path — in its Error()
		// string. Unwrap to the underlying transport error so the token
		// never reaches callers that log or persist Send's error (slog,
		// notification_outbox.last_error).
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("notify: telegram send: %w", urlErr.Err)
		}
		return fmt.Errorf("notify: telegram send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Defensive: scrub the token from the response snippet too, in
		// case some proxy/error page happens to echo the request path.
		snippet := RedactToken(string(respBody), t.Secret)
		return fmt.Errorf("notify: telegram non-2xx status %d: %s", resp.StatusCode, snippet)
	}
	return nil
}
