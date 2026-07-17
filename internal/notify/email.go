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

// EmailConfig — настройки SMTP-отправителя.
type EmailConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

// EmailSender шлёт уведомления по email через SMTP.
type EmailSender struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

// NewEmailSender строит EmailSender из конфигурации.
func NewEmailSender(cfg EmailConfig) *EmailSender {
	return &EmailSender{
		Host:     cfg.Host,
		Port:     cfg.Port,
		User:     cfg.User,
		Password: cfg.Password,
		From:     cfg.From,
	}
}

// Configured сообщает, задан ли SMTP-хост (т.е. отправитель пригоден к
// использованию).
func (s *EmailSender) Configured() bool {
	return s.Host != ""
}

// defaultSMTPDeadline bounds the whole SMTP conversation when ctx carries
// no deadline of its own.
const defaultSMTPDeadline = 30 * time.Second

// Send отправляет письмо на Target.Target с темой и телом из payload.
//
// Written against net.Dialer/smtp.NewClient (rather than the simpler
// smtp.SendMail) specifically to honour ctx: SendMail has no notion of a
// deadline, so a blackholed SMTP server (accepts the TCP connection, then
// never speaks) would stall this call forever. Since Worker.process runs
// jobs sequentially, that stall doesn't just affect the email channel — it
// blocks delivery on every other channel too. Dialing with DialContext and
// then setting a deadline derived from ctx (or defaultSMTPDeadline, if ctx
// carries none) bounds the whole conversation.
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
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(defaultSMTPDeadline))
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
	}

	if s.Password != "" {
		auth := smtp.PlainAuth("", s.User, s.Password, s.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("notify: smtp auth: %w", err)
		}
	}

	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("notify: smtp mail: %w", err)
	}
	if err := c.Rcpt(t.Target); err != nil {
		return fmt.Errorf("notify: smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: smtp data: %w", err)
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

// BuildEmail собирает RFC 5322-подобное сообщение (заголовки + text/plain
// тело). Вынесена отдельно от Send, чтобы тестировать построение письма
// без реального SMTP-соединения.
//
// from/to/subject приходят от пользователя (subject, в частности,
// выводится из issue title) и попадают в сырые заголовки без какого-либо
// экранирования на уровне net/smtp — поэтому каждое значение прогоняется
// через sanitizeHeader перед интерполяцией, чтобы CR/LF не мог
// инъецировать произвольные заголовки (например Bcc) или начать тело
// письма раньше времени.
func BuildEmail(from, to, subject, body string) []byte {
	from = sanitizeHeader(from)
	to = sanitizeHeader(to)
	subject = truncateRunes(sanitizeHeader(subject), maxSubjectRunes)

	htmlBody := buildHTMLBody(subject, body)

	// multipart/alternative: текстовые клиенты видят text/plain, остальные —
	// оформленный HTML. plain-часть пишется как есть (body user-influenced),
	// поэтому boundary генерируется случайно ПОД содержимое и проверяется на
	// отсутствие в теле частей — тело с литеральным boundary иначе могло бы
	// инъецировать/подменить MIME-часть (BE-L1).
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

// makeBoundary генерирует случайный MIME-разделитель (128 бит энтропии),
// гарантированно НЕ встречающийся ни в одной из частей письма. Случайного
// boundary уже достаточно, чтобы user-controlled body не мог его угадать, но
// коллизия проверяется явно и boundary перегенерируется при совпадении —
// так тело, содержащее literal boundary, не способно инъецировать MIME-часть.
func makeBoundary(parts ...string) string {
	for {
		raw := make([]byte, 16)
		// crypto/rand.Read на практике не возвращает ошибку; при сбое просто
		// перегенерируем (следующая итерация даст свежие байты).
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

// buildHTMLBody оборачивает текстовое тело в простой self-contained HTML
// (inline-стили, без внешних ресурсов/картинок). Всё html-экранировано, переносы
// строк сохраняются через <br>. subject уже sanitized/truncated вызывающим.
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

// maxSubjectRunes caps the Subject header so a pathologically long
// user-controlled title can't bloat the message.
const maxSubjectRunes = 200

// sanitizeHeader strips CR and LF from a value destined for a raw RFC 5322
// header line, replacing each with a space. Header values here come from
// user-controlled input (subject derived from issue titles, from/to
// addresses); without this, an embedded "\r\n" terminates the header early
// and lets an attacker inject arbitrary headers or body content.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// truncateRunes caps s at n runes (not bytes, to avoid splitting multi-byte
// UTF-8 sequences).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
