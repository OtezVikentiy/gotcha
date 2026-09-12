package notify

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

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
	if !strings.Contains(msg, "line one\nline two & more") {
		t.Fatalf("plain part missing raw body: %s", msg)
	}
	if !strings.Contains(msg, "line one<br>line two &amp; more") {
		t.Fatalf("html part not escaped/br'd: %s", msg)
	}
	if !strings.Contains(msg, "&lt;ValueError&gt;") {
		t.Fatalf("html subject not escaped: %s", msg)
	}
}

func TestBuildEmailSanitizesHeaderInjection(t *testing.T) {
	msg := string(BuildEmail("f@x", "t@y", "subj\r\nBcc: evil@x", "body"))
	head, _, _ := strings.Cut(msg, "--"+boundaryOf(t, msg))
	if strings.Contains(head, "\r\nBcc:") {
		t.Fatalf("header injection not sanitized: %q", head)
	}
}

func TestBuildEmailBoundaryInjection(t *testing.T) {
	// Атакующий кладёт в тело старый фиксированный токен и попытку новой части.
	evil := "hi\r\n--gotcha_boundary_9f3a2e17c4b8\r\n" +
		"Content-Type: text/html\r\n\r\n<h1>spoof</h1>"
	msg := string(BuildEmail("f@x", "t@y", "subj", evil))

	boundary := boundaryOf(t, msg)
	if strings.Contains(evil, boundary) {
		t.Fatalf("generated boundary collides with body content: %q", boundary)
	}
	if n := strings.Count(msg, "--"+boundary+"\r\n"); n != 2 {
		t.Fatalf("part-delimiter count = %d, want 2 (no injected part): %s", n, msg)
	}
	if n := strings.Count(msg, "--"+boundary+"--\r\n"); n != 1 {
		t.Fatalf("closing-delimiter count = %d, want 1", n)
	}
	if !strings.Contains(msg, evil) {
		t.Fatalf("raw body not preserved verbatim in plain part: %s", msg)
	}
}

func TestSetSMTPDeadlineAppliesCtxDeadline(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := setSMTPDeadline(ctx, c1); err != nil {
		t.Fatalf("setSMTPDeadline: %v", err)
	}
}

func TestSetSMTPDeadlineAppliesDefault(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	if err := setSMTPDeadline(context.Background(), c1); err != nil {
		t.Fatalf("setSMTPDeadline: %v", err)
	}
}

func TestSetSMTPDeadlineErrorPropagates(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	c1.Close() // соединение уже закрыто — SetDeadline на нём обязан отказать

	err := setSMTPDeadline(context.Background(), c1)
	if err == nil {
		t.Fatal("setSMTPDeadline on a closed conn = nil, want error")
	}
}
