package notify

import (
	"strings"
	"testing"
)

// boundaryOf извлекает MIME-boundary из Content-Type письма (теперь он
// генерируется случайно, а не фиксирован).
func boundaryOf(t *testing.T, msg string) string {
	t.Helper()
	_, rest, ok := strings.Cut(msg, `boundary="`)
	if !ok {
		t.Fatalf("no boundary in header: %s", msg)
	}
	b, _, ok := strings.Cut(rest, `"`)
	if !ok || b == "" {
		t.Fatalf("malformed boundary: %s", msg)
	}
	return b
}

func TestBuildEmailMultipart(t *testing.T) {
	msg := string(BuildEmail("from@gotcha.example", "to@corp.com",
		"Alert: <ValueError>", "line one\nline two & more"))

	if !strings.Contains(msg, "Content-Type: multipart/alternative; boundary=\"") {
		t.Fatalf("missing multipart header: %s", msg)
	}
	emailBoundary := boundaryOf(t, msg)
	if !strings.Contains(msg, "--"+emailBoundary+"\r\n") {
		t.Fatalf("missing boundary delimiter: %s", msg)
	}
	if !strings.Contains(msg, "--"+emailBoundary+"--\r\n") {
		t.Fatalf("missing closing boundary: %s", msg)
	}
	if !strings.Contains(msg, "Content-Type: text/plain; charset=\"utf-8\"") ||
		!strings.Contains(msg, "Content-Type: text/html; charset=\"utf-8\"") {
		t.Fatalf("missing one of the parts: %s", msg)
	}
	// Plain-часть содержит исходное тело как есть.
	if !strings.Contains(msg, "line one\nline two & more") {
		t.Fatalf("plain part missing raw body: %s", msg)
	}
	// HTML-часть экранирует спецсимволы и переносит строки.
	if !strings.Contains(msg, "line one<br>line two &amp; more") {
		t.Fatalf("html part not escaped/br'd: %s", msg)
	}
	// Subject html-экранирован в HTML-части (заголовок <ValueError> → &lt;…).
	if !strings.Contains(msg, "&lt;ValueError&gt;") {
		t.Fatalf("html subject not escaped: %s", msg)
	}
}

func TestBuildEmailSanitizesHeaderInjection(t *testing.T) {
	// CR/LF в subject не должны инъецировать заголовки: sanitizeHeader → пробелы.
	msg := string(BuildEmail("f@x", "t@y", "subj\r\nBcc: evil@x", "body"))
	// Заголовочная секция (до первого boundary) не содержит Bcc-строки.
	head, _, _ := strings.Cut(msg, "--"+boundaryOf(t, msg))
	if strings.Contains(head, "\r\nBcc:") {
		t.Fatalf("header injection not sanitized: %q", head)
	}
}

// BE-L1: тело, содержащее literal boundary-токен, не должно инъецировать
// MIME-часть. Boundary генерируется случайно под содержимое, поэтому даже
// подстановка старого фиксированного токена в тело не совпадает с реальным
// разделителем — число частей остаётся ровно 2 (text/plain + text/html).
func TestBuildEmailBoundaryInjection(t *testing.T) {
	// Атакующий кладёт в тело старый фиксированный токен и попытку новой части.
	evil := "hi\r\n--gotcha_boundary_9f3a2e17c4b8\r\n" +
		"Content-Type: text/html\r\n\r\n<h1>spoof</h1>"
	msg := string(BuildEmail("f@x", "t@y", "subj", evil))

	boundary := boundaryOf(t, msg)
	// Реальный boundary не встречается в подставленном пользователем тексте.
	if strings.Contains(evil, boundary) {
		t.Fatalf("generated boundary collides with body content: %q", boundary)
	}
	// Ровно два открывающих разделителя (--boundary\r\n) — тело не добавило часть.
	if n := strings.Count(msg, "--"+boundary+"\r\n"); n != 2 {
		t.Fatalf("part-delimiter count = %d, want 2 (no injected part): %s", n, msg)
	}
	if n := strings.Count(msg, "--"+boundary+"--\r\n"); n != 1 {
		t.Fatalf("closing-delimiter count = %d, want 1", n)
	}
	// Тело пользователя присутствует целиком как контент plain-части, а не как структура.
	if !strings.Contains(msg, evil) {
		t.Fatalf("raw body not preserved verbatim in plain part: %s", msg)
	}
}
