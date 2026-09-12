package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"gitflic.ru/otezvikentiy/gotcha/internal/netguard"
)

type WebhookSender struct {
	Client       *http.Client
	AllowPrivate bool

	safeOnce   sync.Once
	safeClient *http.Client
}

func (s *WebhookSender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	s.safeOnce.Do(func() {
		s.safeClient = netguard.SafeHTTPClient(s.AllowPrivate, httpClientTimeout)
	})
	return s.safeClient
}

// secret — ключ HMAC-подписи; эти поля не должны попасть в тело вебхука.
var transportFields = map[string]struct{}{
	"channel_kind": {},
	"target":       {},
	"secret":       {},
}

func (s *WebhookSender) Send(ctx context.Context, t Target, payload map[string]any) error {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if _, skip := transportFields[k]; skip {
			continue
		}
		out[k] = v
	}
	body, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("notify: webhook marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Target, bytes.NewReader(body))
	if err != nil {
		// *url.Error несёт полный URL цели с токеном/секретом в пути или query —
		// распаковываем, чтобы он не утёк в лог через обёрнутую ошибку.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("notify: webhook request: %w", urlErr.Err)
		}
		return fmt.Errorf("notify: webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Secret != "" {
		mac := hmac.New(sha256.New, []byte(t.Secret))
		mac.Write(body)
		req.Header.Set("X-Gotcha-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := s.client().Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("notify: webhook send: %w", urlErr.Err)
		}
		return fmt.Errorf("notify: webhook send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		snippet := RedactToken(string(respBody), t.Target)
		if u, perr := url.Parse(t.Target); perr == nil && u.Path != "" && u.Path != "/" {
			snippet = RedactToken(snippet, u.Path)
		}
		return fmt.Errorf("notify: webhook non-2xx status %d: %s", resp.StatusCode, snippet)
	}
	return nil
}
