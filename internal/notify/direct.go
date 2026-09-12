package notify

import (
	"context"
	"fmt"
	"time"
)

// Синхронно, минуя outbox: результат теста канала нужен немедленно, очередь
// с ретраями размазала бы его во времени. Общая механика с Worker.process.
type Direct struct {
	Senders map[string]Sender
	Secrets SecretResolver
	// 0 → defaultSendTimeout.
	SendTimeout time.Duration
}

// Ошибка возвращается как есть; ретраев здесь нет намеренно.
func (d *Direct) Send(ctx context.Context, channelID int64, kind, target string, payload map[string]any) error {
	sender, ok := d.Senders[kind]
	if !ok {
		return fmt.Errorf("notify: no sender registered for channel kind %q", kind)
	}
	t := Target{Kind: kind, Target: target}
	// Секрет по channel_id, не из payload — не должен жить в jsonb. Kind сравниваем
	// с литералом "email", не alert.ChannelEmail — импорт назад замкнул бы цикл.
	if d.Secrets != nil && kind != "email" {
		secret, err := d.Secrets.ChannelSecret(ctx, channelID)
		if err != nil {
			return fmt.Errorf("notify: resolve channel %d secret: %w", channelID, err)
		}
		t.Secret = secret
	}
	timeout := d.SendTimeout
	if timeout <= 0 {
		timeout = defaultSendTimeout
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return sender.Send(sendCtx, t, payload)
}
