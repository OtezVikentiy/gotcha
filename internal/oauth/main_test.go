package oauth

import (
	"os"
	"testing"
)

// TestMain разрешает приватные/loopback адреса для тестов: провайдеры бьют по
// httptest-серверам на 127.0.0.1, а sharedClient по умолчанию SSRF-safe (режет
// loopback). В проде фильтр остаётся включён (SetAllowPrivateHosts из main.go).
func TestMain(m *testing.M) {
	SetAllowPrivateHosts(true)
	os.Exit(m.Run())
}
