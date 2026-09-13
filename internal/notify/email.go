package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type EmailConfig struct {
	Host       string
	Port       int
	User       string
	Password   string
	From       string
	RequireTLS bool
}

type EmailSender struct {
	Host       string
	Port       int
	User       string
	Password   string
	From       string
	RequireTLS bool
}

func NewEmailSender(cfg EmailConfig) *EmailSender {
	return &EmailSender{
		Host:       cfg.Host,
		Port:       cfg.Port,
		User:       cfg.User,
		Password:   cfg.Password,
		From:       cfg.From,
		RequireTLS: cfg.RequireTLS,
	}
}

func (s *EmailSender) Configured() bool {
	return s.Host != ""
}

// Used when ctx carries no deadline of its own.
const defaultSMTPDeadline = 30 * time.Second

// Extracted so the failure path is testable without a live server: a real
// conn can race closed between dial and here — failing fast catches it early.
func setSMTPDeadline(ctx context.Context, conn net.Conn) error {
	deadline := time.Now().Add(defaultSMTPDeadline)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	return conn.SetDeadline(deadline)
}

// smtp.SendMail ignores ctx; a blackholed server would stall forever and,
// since Worker.process runs jobs sequentially, block every other channel too.
func (s *EmailSender) Send(ctx context.Context, t Target, payload map[string]any) error {
	subject, _ := payload["subject"].(string)
	body, _ := payload["body"].(string)
	msg := BuildEmail(s.From, t.Target, subject, body)

	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("notify: dial smtp: %w", err)
	}
	if err := setSMTPDeadline(ctx, conn); err != nil {
		conn.Close()
		return fmt.Errorf("notify: set smtp deadline: %w", err)
	}

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("notify: smtp client: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return fmt.Errorf("notify: smtp starttls: %w", err)
		}
	} else if s.RequireTLS {
		// Сервер не предложил STARTTLS в EHLO — без этой проверки письмо (и, если
		// задан пароль, PlainAuth) ушло бы открытым текстом при активной подмене ответа.
		return fmt.Errorf("notify: smtp starttls required but not offered by server")
	}

	if s.Password != "" {
		auth := smtp.PlainAuth("", s.User, s.Password, s.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("notify: smtp auth: %w", err)
		}
	}

	if err := c.Mail(s.From); err != nil {
		return wrapSMTPErr("mail", err, t.Target)
	}
	if err := c.Rcpt(t.Target); err != nil {
		return wrapSMTPErr("rcpt", err, t.Target)
	}
	w, err := c.Data()
	if err != nil {
		return wrapSMTPErr("data", err, t.Target)
	}
	if _, err := w.Write(msg); err != nil {
		w.Close()
		return fmt.Errorf("notify: smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: smtp write close: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("notify: smtp quit: %w", err)
	}
	return nil
}

// Redacts the recipient from a raw SMTP-stage error before wrapping — servers
// often echo it in rejection replies, which end up in notification_outbox.last_error.
func wrapSMTPErr(stage string, err error, recipient string) error {
	return fmt.Errorf("notify: smtp %s: %s", stage, RedactToken(err.Error(), recipient))
}

// from/to/subject приходят от пользователя без экранирования в net/smtp —
// каждое прогоняется через sanitizeHeader, чтобы CR/LF не инъецировал заголовки.
func BuildEmail(from, to, subject, body string) []byte {
	from = sanitizeHeader(from)
	to = sanitizeHeader(to)
	subject = truncateRunes(sanitizeHeader(subject), maxSubjectRunes)

	htmlBody := buildHTMLBody(subject, body)

	// boundary генерируется под содержимое письма и проверяется на отсутствие в
	// нём — иначе тело могло бы подменить MIME-часть.
	boundary := makeBoundary(body, htmlBody)

	headers := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n",
		from, to, subject, boundary)

	var b strings.Builder
	b.WriteString(headers)
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(body)
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

// 128 бит энтропии; коллизия с частями письма проверяется явно и boundary
// перегенерируется — тело с literal boundary не инъецирует MIME-часть.
func makeBoundary(parts ...string) string {
	for {
		raw := make([]byte, 16)
		// crypto/rand.Read практически не ошибается; при сбое просто перегенерируем.
		if _, err := rand.Read(raw); err != nil {
			continue
		}
		cand := "gotcha_boundary_" + hex.EncodeToString(raw)
		collision := false
		for _, p := range parts {
			if strings.Contains(p, cand) {
				collision = true
				break
			}
		}
		if !collision {
			return cand
		}
	}
}

// subject уже sanitized/truncated вызывающим.
func buildHTMLBody(subject, body string) string {
	escBody := strings.ReplaceAll(html.EscapeString(body), "\n", "<br>")
	escSubject := html.EscapeString(subject)
	return `<!doctype html><html><body style="margin:0;padding:16px;font-family:-apple-system,Segoe UI,Roboto,Arial,sans-serif;color:#1a1a1a;background:#ffffff">` +
		`<div style="max-width:560px;margin:0 auto">` +
		`<h2 style="font-size:16px;margin:0 0 12px;color:#111">` + escSubject + `</h2>` +
		`<div style="font-size:14px;line-height:1.5;color:#333">` + escBody + `</div>` +
		`<hr style="border:none;border-top:1px solid #e5e5e5;margin:20px 0">` +
		`<p style="font-size:12px;color:#999;margin:0">— Gotcha</p>` +
		`</div></body></html>`
}

// Caps Subject so a pathologically long user-controlled title can't bloat the message.
const maxSubjectRunes = 200

// Strips CR/LF from user-controlled header values (subject, from/to) — an
// embedded \r\n could otherwise terminate the header early and inject others.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// Runes, not bytes — avoids splitting multi-byte UTF-8 sequences.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
