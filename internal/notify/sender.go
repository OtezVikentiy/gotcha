package notify

import (
	"context"
	"net/http"
	"time"
)

// Target — минимальные данные о канале доставки, нужные отправителю.
// notify не импортирует alert (чтобы не создавать цикл alert -> notify ->
// alert); alert.Channel конвертируется в Target вызывающей стороной перед
// Enqueue (значения кладутся в payload как channel_kind/target/secret и
// восстанавливаются воркером).
type Target struct {
	Kind   string
	Target string
	Secret string
}

// Sender отправляет одно уведомление через конкретный канал.
type Sender interface {
	Send(ctx context.Context, t Target, payload map[string]any) error
}

// httpClientTimeout bounds requests made by the shared default HTTP
// client. http.DefaultClient has no timeout, so a single hanging target
// (dead peer, blackholed network) would otherwise tie up a Send call
// indefinitely.
const httpClientTimeout = 15 * time.Second

var sharedDefaultClient = &http.Client{Timeout: httpClientTimeout}

// defaultClient returns the HTTP client senders fall back to when no
// Client is configured explicitly. Shared (not http.DefaultClient) so
// every sender gets a bounded timeout without each duplicating the same
// client construction.
func defaultClient() *http.Client {
	return sharedDefaultClient
}
