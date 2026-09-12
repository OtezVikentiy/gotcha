package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPackageBuilds(t *testing.T) {}

// код выхода 2 общий с ошибкой конфига на обычном запуске: от него зависит
// RestartPreventExitStatus=2 в systemd-юните
func TestCheckSubcommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gotcha-agent")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	t.Run("валидный конфиг — выход 0", func(t *testing.T) {
		cmd := exec.Command(bin, "--check")
		cmd.Env = append(os.Environ(),
			"GOTCHA_AGENT_ENDPOINT=https://gotcha.example",
			"GOTCHA_AGENT_INGEST_KEY=test-key",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("--check на валидном конфиге: %v\n%s", err, out)
		}
	})

	t.Run("битый конфиг — выход 2", func(t *testing.T) {
		cmd := exec.Command(bin, "--check")
		cmd.Env = append(os.Environ(), "GOTCHA_AGENT_ENDPOINT=", "GOTCHA_AGENT_INGEST_KEY=")
		out, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("--check на пустом конфиге: err = %v (ожидался *exec.ExitError), вывод:\n%s", err, out)
		}
		if code := exitErr.ExitCode(); code != 2 {
			t.Errorf("--check на пустом конфиге: код выхода = %d, want 2\n%s", code, out)
		}
	})
}
