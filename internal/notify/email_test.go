package notify

import (
	"strings"
	"testing"
)

func TestBuildEmailMultipart(t *testing.T) {
	msg := string(BuildEmail("from@gotcha.example", "to@corp.com",
		"Alert: <ValueError>", "line one\nline two & more"))

	if !strings.Contains(msg, "Content-Type: multipart/alternative; boundary=\"") {
		t.Fatalf("missing multipart header: %s", msg)
	}
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
	head, _, _ := strings.Cut(msg, "--"+emailBoundary)
	if strings.Contains(head, "\r\nBcc:") {
		t.Fatalf("header injection not sanitized: %q", head)
	}
}
