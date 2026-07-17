package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
)

// regForm — валидная форма регистрации для указанного email.
func regForm(email string) url.Values {
	return url.Values{
		"email":     {email},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
}

// TestRegister_Mode проверяет гейтинг регистрации по режиму с bootstrap-исключением
// для первого пользователя инстанса (PROD-B1).
func TestRegister_Mode(t *testing.T) {
	// open: регистрация проходит даже когда пользователи уже есть.
	t.Run("open allows subsequent", func(t *testing.T) {
		s := newStack(t)
		s.h.RegistrationMode = "open"
		// Первый (bootstrap-админ).
		resp := postForm(t, s.srv, "/register", regForm("open-a@example.com"), s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("first register status = %d, want 303", resp.StatusCode)
		}
		// Второй — тоже проходит.
		resp = postForm(t, s.srv, "/register", regForm("open-b@example.com"), s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("second register (open) status = %d, want 303", resp.StatusCode)
		}
		if n, _ := s.h.Auth.UserCount(context.Background()); n != 2 {
			t.Fatalf("UserCount = %d, want 2", n)
		}
	})

	// invite: первый может (bootstrap), второй — 403 и Register не выполнен.
	t.Run("invite blocks second, allows first", func(t *testing.T) {
		s := newStack(t)
		s.h.RegistrationMode = "invite"
		// Первый (bootstrap) — проходит несмотря на invite.
		resp := postForm(t, s.srv, "/register", regForm("inv-a@example.com"), s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("bootstrap register status = %d, want 303", resp.StatusCode)
		}
		// Второй — запрещён.
		resp = postForm(t, s.srv, "/register", regForm("inv-b@example.com"), s.srv.URL, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("second register (invite) status = %d, want 403", resp.StatusCode)
		}
		if sessionCookie(resp) != nil {
			t.Fatalf("second register (invite) must not set session cookie")
		}
		if n, _ := s.h.Auth.UserCount(context.Background()); n != 1 {
			t.Fatalf("UserCount = %d, want 1 (Register must not run)", n)
		}
		_ = body
	})

	// closed: второй — 403.
	t.Run("closed blocks second", func(t *testing.T) {
		s := newStack(t)
		s.h.RegistrationMode = "closed"
		resp := postForm(t, s.srv, "/register", regForm("cl-a@example.com"), s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("bootstrap register (closed) status = %d, want 303", resp.StatusCode)
		}
		resp = postForm(t, s.srv, "/register", regForm("cl-b@example.com"), s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("second register (closed) status = %d, want 403", resp.StatusCode)
		}
		if n, _ := s.h.Auth.UserCount(context.Background()); n != 1 {
			t.Fatalf("UserCount = %d, want 1", n)
		}
	})
}
