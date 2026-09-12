package notify

import (
	"context"
	"net/http"
	"time"
)

// notify не импортирует alert (цикл alert → notify → alert); alert.Channel
// конвертируется в Target вызывающей стороной перед Enqueue.
type Target struct {
	Kind   string
	Target string
	Secret string
}

type Sender interface {
	Send(ctx context.Context, t Target, payload map[string]any) error
}

// http.DefaultClient has no timeout — a hanging target would tie up Send
// indefinitely.
const httpClientTimeout = 15 * time.Second

var sharedDefaultClient = &http.Client{Timeout: httpClientTimeout}

// Shared, not http.DefaultClient, so every sender gets a bounded timeout
// without duplicating client construction.
func defaultClient() *http.Client {
	return sharedDefaultClient
}
