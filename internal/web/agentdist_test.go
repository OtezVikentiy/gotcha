package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeAgentBinary кладёт в dir один allowlisted файл с заданным
// содержимым — тест не различает реальный бинарь и фикстуру, важна только
// стабильность содержимого для sha256-ETag.
func writeFakeAgentBinary(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeFakeAgentBinary(%s): %v", name, err)
	}
}

// newAgentDistServer поднимает Handler с Register(mux) за httptest.Server —
// без PG/CH: install.sh и /agent/{file} не трогают ни одну из служб, только
// AgentDistDir на диске. Через настоящий сервер (не прямой вызов хендлера)
// проходит и штатное path-cleaning net/http (редирект ".." до catch-all 404).
func newAgentDistServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	h := &Handler{AgentDistDir: dir, BaseURL: "http://localhost"}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallShServed(t *testing.T) {
	dir := t.TempDir()
	writeFakeAgentBinary(t, dir, "gotcha-agent-linux-amd64", "fake-amd64-binary")
	srv := newAgentDistServer(t, dir)

	resp, err := http.Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatalf("GET /install.sh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/x-sh" {
		t.Errorf("Content-Type = %q, want text/x-sh", ct)
	}
	body := readAll(t, resp)
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Errorf("body does not start with shebang:\n%s", body)
	}

	// Обход allowlist через "..": net/http сам чистит путь и редиректит на
	// /etc/passwd — там ловит catch-all "/" и отдаёт 404. Ни один файл вне
	// AgentDistDir наружу не уходит.
	resp2, err := http.Get(srv.URL + "/agent/../../etc/passwd")
	if err != nil {
		t.Fatalf("GET traversal: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("traversal status = %d, want 404", resp2.StatusCode)
	}

	// Имя вне allowlist-мапы — тоже 404, даже если файл с таким именем
	// физически лежит в AgentDistDir.
	writeFakeAgentBinary(t, dir, "evil", "should never be served")
	resp3, err := http.Get(srv.URL + "/agent/evil")
	if err != nil {
		t.Fatalf("GET /agent/evil: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("/agent/evil status = %d, want 404", resp3.StatusCode)
	}
}

func TestAgentDistNoDir(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{"empty AgentDistDir", ""},
		{"non-existent AgentDistDir", filepath.Join(t.TempDir(), "does-not-exist")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAgentDistServer(t, tc.dir)

			resp, err := http.Get(srv.URL + "/install.sh")
			if err != nil {
				t.Fatalf("GET /install.sh: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("/install.sh status = %d, want 404", resp.StatusCode)
			}
			if body := readAll(t, resp); !strings.Contains(body, "agent binaries are not bundled in this build") {
				t.Errorf("/install.sh body missing hint:\n%s", body)
			}

			resp2, err := http.Get(srv.URL + "/agent/gotcha-agent-linux-amd64")
			if err != nil {
				t.Fatalf("GET /agent/...: %v", err)
			}
			defer resp2.Body.Close()
			if resp2.StatusCode != http.StatusNotFound {
				t.Errorf("/agent/... status = %d, want 404", resp2.StatusCode)
			}
			if body := readAll(t, resp2); !strings.Contains(body, "agent binaries are not bundled in this build") {
				t.Errorf("/agent/... body missing hint:\n%s", body)
			}
		})
	}
}

func TestAgentFileHeaders(t *testing.T) {
	dir := t.TempDir()
	writeFakeAgentBinary(t, dir, "gotcha-agent-linux-amd64", "fake-amd64-binary-content")
	srv := newAgentDistServer(t, dir)

	resp, err := http.Get(srv.URL + "/agent/gotcha-agent-linux-amd64")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("ETag пуст")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (обязан перекрыть no-store от securityHeaders)", cc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/agent/gotcha-agent-linux-amd64", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET If-None-Match: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("повтор с If-None-Match: status = %d, want 304", resp2.StatusCode)
	}
}

// TestAgentDistRateLimited проверяет, что /agent/{file} и /install.sh резаны
// НЕЗАВИСИМЫМИ лимитерами (agentLimiter и publicLimiter соответственно,
// см. Handler.agentLimiter): исчерпание одного не задевает другой роут.
func TestAgentDistRateLimited(t *testing.T) {
	dir := t.TempDir()
	writeFakeAgentBinary(t, dir, "gotcha-agent-linux-amd64", "fake-amd64-binary")

	t.Run("agentLimiter режет /agent, не трогая /install.sh", func(t *testing.T) {
		h := &Handler{
			AgentDistDir:  dir,
			BaseURL:       "http://localhost",
			publicLimiter: newRateLimiter(time.Now, 600, time.Minute),
			agentLimiter:  newRateLimiter(time.Now, 0, time.Minute),
		}
		mux := http.NewServeMux()
		h.Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + "/agent/gotcha-agent-linux-amd64")
		if err != nil {
			t.Fatalf("GET /agent/...: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("/agent/... status = %d, want 429", resp.StatusCode)
		}

		resp2, err := http.Get(srv.URL + "/install.sh")
		if err != nil {
			t.Fatalf("GET /install.sh: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Errorf("/install.sh status = %d, want 200 (не должен резаться исчерпанным agentLimiter)", resp2.StatusCode)
		}
	})

	t.Run("publicLimiter режет /install.sh, не трогая /agent", func(t *testing.T) {
		h := &Handler{
			AgentDistDir:  dir,
			BaseURL:       "http://localhost",
			publicLimiter: newRateLimiter(time.Now, 0, time.Minute),
			agentLimiter:  newRateLimiter(time.Now, 10, time.Minute),
		}
		mux := http.NewServeMux()
		h.Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + "/install.sh")
		if err != nil {
			t.Fatalf("GET /install.sh: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("/install.sh status = %d, want 429", resp.StatusCode)
		}

		resp2, err := http.Get(srv.URL + "/agent/gotcha-agent-linux-amd64")
		if err != nil {
			t.Fatalf("GET /agent/...: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Errorf("/agent/... status = %d, want 200 (не должен резаться исчерпанным publicLimiter)", resp2.StatusCode)
		}
	})
}

// TestInstallScriptInvariants — сторож против случайной правки install.sh:
// ключевые механизмы (обёртка main, сверка целостности, права конфига,
// hardening юнита) не должны выпасть при рефакторинге скрипта.
func TestInstallScriptInvariants(t *testing.T) {
	s := string(installShScript)
	for _, want := range []string{
		"main() {", // обёртка: обрыв стриминга не исполняет префикс
		`main "$@"`,
		"sha256sum -c", // сверка целостности
		"gotcha-agent.service",
		"install -m 600", // конфиг 0600 root:root ДО записи ключа
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=read-only",       // не yes: не прятать /var/tmp как раздел (ops-H5/sec-M2)
		"RestartPreventExitStatus=2",  // код 2 = ошибка конфига — рестарт её не лечит (ops-H3)
		"--check",                     // валидация конфига до enable (ops-H2)
		"systemctl is-active --quiet", // подтверждение, что процесс реально жив (ops-H2)
		"Nice=10",                     // проба процессов не должна конкурировать за CPU (ops-MED)
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh потерял инвариант %q", want)
		}
	}
	if strings.Contains(s, "ProtectProc") || strings.Contains(s, "ProcSubset") {
		t.Error("ProtectProc/ProcSubset ломают чтение /proc (спека §2.3)")
	}
	// Ищем именно директиву юнита ("PrivateTmp=..."), а не слово "PrivateTmp" —
	// оно легитимно встречается в поясняющем комментарии рядом с ProtectHome.
	if strings.Contains(s, "PrivateTmp=") {
		t.Error("PrivateTmp маскирует /var/tmp как отдельный раздел — выпадает из порога «Диск» (ops-H5/sec-M2)")
	}
	if strings.Contains(s, "ProtectHome=yes") {
		t.Error("ProtectHome=yes прячет /home,/root,/var/tmp под tmpfs (ops-H5/sec-M2)")
	}
}

// readAll читает тело ответа целиком — пакет web_test держит одноимённый
// хелпер (tenancy_revoke_test.go), но это разные пакеты (package web vs
// package web_test), общий код между ними не виден.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
