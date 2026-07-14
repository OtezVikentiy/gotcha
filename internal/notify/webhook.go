package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WebhookSender шлёт уведомление как JSON POST на Target.Target, подписывая
// тело HMAC-SHA256(Secret) в заголовке X-Gotcha-Signature, если Secret
// задан.
type WebhookSender struct {
	Client *http.Client
}

func (s *WebhookSender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient()
}

// transportFields — keys that alert.Evaluator stuffs into the outbox
// payload purely so the worker can rebuild a notify.Target (see
// worker.go's process: channel_kind/target/secret feed Target.Kind/
// Target.Target/Target.Secret). They describe the delivery transport, not
// the alert itself, and must never be echoed in the outbound body —
// "secret" in particular is the HMAC signing key, so forwarding it would
// let anyone reading the receiver's logs forge signed webhook requests.
var transportFields = map[string]struct{}{
	"channel_kind": {},
	"target":       {},
	"secret":       {},
}

// Send отправляет payload как JSON на t.Target, с транспортными полями
// (channel_kind/target/secret — см. transportFields) вырезанными из тела:
// они существуют только для внутреннего использования воркером и не
// предназначены для получателя вебхука.
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
		return fmt.Errorf("notify: webhook send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("notify: webhook non-2xx status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
