package notify

import (
	"context"
	"fmt"
	"time"
)

// Direct — синхронная доставка в один канал, минуя outbox (№69: тестовое
// сообщение из настроек канала — результат нужен немедленно, очередь с
// ретраями только размазала бы его во времени и спрятала бы ошибку).
// Механика выбора отправителя, секрета и таймаута — общая с Worker.process:
// обе двери ведут к одним и тем же Sender'ам, и тест канала проверяет ровно
// тот путь, которым пойдёт настоящий алерт.
type Direct struct {
	Senders map[string]Sender
	Secrets SecretResolver
	// SendTimeout — бюджет одной отправки; 0 → defaultSendTimeout.
	SendTimeout time.Duration
}

// Send доставляет payload в канал channelID вида kind по адресу target.
// Ошибка возвращается вызывающему как есть — ему решать, что показать
// человеку; ретраев здесь нет намеренно.
func (d *Direct) Send(ctx context.Context, channelID int64, kind, target string, payload map[string]any) error {
	sender, ok := d.Senders[kind]
	if !ok {
		return fmt.Errorf("notify: no sender registered for channel kind %q", kind)
	}
	t := Target{Kind: kind, Target: target}
	// Секрет — по channel_id в момент отправки, не из payload (та же
	// причина, что у Worker.process: bot-токен не должен жить в jsonb).
	// Вид сравниваем с литералом, а не с alert.ChannelEmail: зависимость
	// идёт alert → notify, и импорт в обратную сторону замкнул бы цикл.
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
