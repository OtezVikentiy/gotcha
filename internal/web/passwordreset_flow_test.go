package web_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Захватывает тело последнего письма (DATA) и разблокирует done, когда обмен завершён —
// тест ждёт done, а не спит наугад, и не оставляет фоновую горутину жить дольше теста.
func fakeCapturingSMTP(t *testing.T) (host string, port int, body <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tp := textproto.NewConn(conn)
		_ = tp.PrintfLine("220 fake.smtp ready")
		var data strings.Builder
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_ = tp.PrintfLine("250 fake.smtp")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				_ = tp.PrintfLine("250 OK")
			case line == "DATA":
				_ = tp.PrintfLine("354 go ahead")
				for {
					b, err := tp.ReadLine()
					if err != nil {
						return
					}
					if b == "." {
						break
					}
					data.WriteString(b)
					data.WriteString("\n")
				}
				_ = tp.PrintfLine("250 OK")
			case strings.HasPrefix(line, "QUIT"):
				_ = tp.PrintfLine("221 bye")
				out <- data.String()
				return
			default:
				_ = tp.PrintfLine("250 OK")
			}
		}
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	portNum, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return h, portNum, out
}

var resetLinkRe = regexp.MustCompile(`http://[^\s]+/reset-password/([A-Za-z0-9_-]+)`)

// Сквозной сценарий: запрос -> письмо со ссылкой -> новый пароль -> старые сессии убиты,
// старый пароль не работает, новый работает -> повторное использование ссылки отклонено.
func TestPasswordResetFullFlow(t *testing.T) {
	s := newStack(t)
	host, port, emailBody := fakeCapturingSMTP(t)
	s.h.Email = notify.NewEmailSender(notify.EmailConfig{Host: host, Port: port, From: "noreply@gotcha.test"})
	s.h.EmailEnabled = true

	ctx := context.Background()
	if _, err := s.h.Auth.Register(ctx, "flow@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	uid, err := s.h.Auth.UserByEmail(ctx, "flow@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	staleSession, err := s.h.Auth.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 1. Запрос восстановления.
	resp := postForm(t, s.srv, "/forgot-password", url.Values{"email": {"flow@example.com"}}, s.srv.URL, nil)
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /forgot-password status = %d, want 200", resp.StatusCode)
	}

	// 2. Письмо приходит с рабочей ссылкой; сам ответ на запрос ссылку/токен не несёт —
	// требование «ссылка не показывается в интерфейсе».
	var mail string
	select {
	case mail = <-emailBody:
	case <-time.After(10 * time.Second):
		t.Fatal("письмо не пришло за отведённое время")
	}
	m := resetLinkRe.FindStringSubmatch(mail)
	if m == nil {
		t.Fatalf("в письме не найдена ссылка восстановления: %q", mail)
	}
	token := m[1]
	if strings.Contains(string(respBody), token) {
		t.Errorf("токен восстановления попал в ответ на POST /forgot-password: %s", respBody)
	}

	// 3. Установка нового пароля по ссылке.
	resetResp := postForm(t, s.srv, "/reset-password/"+token,
		url.Values{"new": {"brand-new-password-1"}, "new2": {"brand-new-password-1"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resetResp.Body)
	resetResp.Body.Close()
	if resetResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /reset-password status = %d, want 303", resetResp.StatusCode)
	}
	if loc := resetResp.Header.Get("Location"); loc != "/login" {
		t.Errorf("редирект после сброса = %q, want /login", loc)
	}

	// 4. Прежняя сессия убита, старый пароль не работает, новый работает.
	if _, err := s.h.Auth.SessionUser(ctx, staleSession); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("сессия пережила сброс пароля: err=%v, want ErrNoSession", err)
	}
	if _, err := s.h.Auth.Authenticate(ctx, "flow@example.com", "old-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("старый пароль всё ещё работает: %v", err)
	}
	if _, err := s.h.Auth.Authenticate(ctx, "flow@example.com", "brand-new-password-1"); err != nil {
		t.Errorf("новый пароль не работает: %v", err)
	}

	// 5. Одноразовость: повторное использование той же ссылки отклоняется.
	reuseResp := postForm(t, s.srv, "/reset-password/"+token,
		url.Values{"new": {"another-password-1"}, "new2": {"another-password-1"}}, s.srv.URL, nil)
	io.Copy(io.Discard, reuseResp.Body)
	reuseResp.Body.Close()
	if reuseResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("повторное использование ссылки: status = %d, want 422", reuseResp.StatusCode)
	}
}
