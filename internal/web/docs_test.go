package web_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

func TestDocs(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "docs-user@example.com")

	resp := getWithCookie(t, s.srv, "/docs", cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Документация") {
		t.Fatalf("GET /docs body missing index title: %s", body)
	}
	if !strings.Contains(string(body), "Термины") {
		t.Fatalf("GET /docs body missing glossary page title: %s", body)
	}
	if !strings.Contains(string(body), `href="/docs/glossary"`) {
		t.Fatalf("GET /docs body missing link to glossary: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/docs/glossary", cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs/glossary status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), ">Термины</h1>") {
		t.Fatalf("GET /docs/glossary body missing rendered H1: %s", body)
	}
	if !strings.Contains(string(body), "<h1 id=") {
		t.Fatalf("у заголовка нет якоря — ссылка на раздел не работает: %s", body)
	}
	if !strings.Contains(string(body), `class="doc-content"`) {
		t.Fatalf("GET /docs/glossary body missing .doc-content article: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/docs/does-not-exist", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /docs/does-not-exist status = %d, want 404", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, "/docs", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /docs (no cookie) status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Fdocs" {
		t.Fatalf("GET /docs (no cookie) Location = %q, want /login?next=%%2Fdocs", loc)
	}

	resp = getWithCookie(t, s.srv, "/docs/glossary", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /docs/glossary (no cookie) status = %d, want 303", resp.StatusCode)
	}
}
