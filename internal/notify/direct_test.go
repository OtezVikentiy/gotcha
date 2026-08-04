package notify

import (
	"context"
	"errors"
	"testing"
)

type recordingSender struct {
	target  Target
	payload map[string]any
	err     error
}

func (s *recordingSender) Send(_ context.Context, t Target, payload map[string]any) error {
	s.target, s.payload = t, payload
	return s.err
}

type staticSecrets string

func (s staticSecrets) ChannelSecret(context.Context, int64) (string, error) {
	return string(s), nil
}

// TestDirectSend — синхронная доставка (№69): выбирает отправителя по kind,
// подкладывает секрет (кроме email), возвращает ошибку отправителя как есть;
// незнакомый kind — ошибка, а не паника.
func TestDirectSend(t *testing.T) {
	rec := &recordingSender{}
	d := &Direct{Senders: map[string]Sender{"telegram": rec}, Secrets: staticSecrets("tok")}
	payload := map[string]any{"subject": "s"}

	if err := d.Send(context.Background(), 7, "telegram", "123", payload); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.target.Kind != "telegram" || rec.target.Target != "123" || rec.target.Secret != "tok" {
		t.Errorf("target = %+v", rec.target)
	}
	if rec.payload["subject"] != "s" {
		t.Errorf("payload = %+v", rec.payload)
	}

	if err := d.Send(context.Background(), 7, "carrier-pigeon", "x", payload); err == nil {
		t.Error("незнакомый kind: want error")
	}

	rec.err = errors.New("boom")
	if err := d.Send(context.Background(), 7, "telegram", "123", payload); err == nil || !errors.Is(err, rec.err) {
		t.Errorf("ошибка отправителя не вернулась как есть: %v", err)
	}
}
