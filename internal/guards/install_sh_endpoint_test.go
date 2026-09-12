package guards

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Гейт в интерфейсе не защищает оператора, взявшего install-команду мимо интерфейса —
// из тикета или плейбука, поэтому проверка обязана быть и в самом скрипте.
func runInstallSh(t *testing.T, root, endpoint string) (output string, err error) {
	t.Helper()
	if _, lookErr := exec.LookPath("sh"); lookErr != nil {
		t.Skip("sh недоступен в PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(root, "internal", "web", "install.sh"))
	cmd.Env = append([]string{}, "PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"GOTCHA_AGENT_ENDPOINT="+endpoint,
		"GOTCHA_AGENT_INGEST_KEY=pk_test",
	)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

func TestInstallShRejectsPlainHTTPOnNonLocalHost(t *testing.T) {
	tree := Load(t)
	out, err := runInstallSh(t, tree.Root, "http://gotcha.example.invalid")
	if err == nil {
		t.Fatalf("install.sh с http://gotcha.example.invalid завершился успешно, ожидался отказ; вывод:\n%s", out)
	}
	const want = "is plain HTTP on a non-local host"
	if !strings.Contains(out, want) {
		t.Fatalf("install.sh не отказал по схеме endpoint (ожидалась подстрока %q в выводе):\n%s", want, out)
	}
}

func TestInstallShAllowsLocalHTTPEndpoint(t *testing.T) {
	tree := Load(t)
	srv := httptest.NewServer(nil)
	url := srv.URL // 127.0.0.1:<port>, ещё живой
	srv.Close()    // порт закрыт: curl упрётся в connection refused, не в наш гейт схемы

	out, err := runInstallSh(t, tree.Root, url)
	if err == nil {
		t.Fatalf("install.sh с закрытым локальным портом завершился успешно, ожидался отказ на скачивании; вывод:\n%s", out)
	}
	const rejected = "is plain HTTP on a non-local host"
	if strings.Contains(out, rejected) {
		t.Fatalf("install.sh отверг localhost-endpoint по схеме — гейт не должен трогать local/127.0.0.1/::1:\n%s", out)
	}
}

func TestInstallShAllowsHTTPSOnNonLocalHost(t *testing.T) {
	tree := Load(t)
	out, err := runInstallSh(t, tree.Root, "https://gotcha.example.invalid")
	if err == nil {
		t.Fatalf("install.sh с https://gotcha.example.invalid завершился успешно, ожидался отказ на скачивании (не на схеме); вывод:\n%s", out)
	}
	const rejected = "is plain HTTP on a non-local host"
	if strings.Contains(out, rejected) {
		t.Fatalf("install.sh отверг https:// endpoint по схеме — не должен:\n%s", out)
	}
}
