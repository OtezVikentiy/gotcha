package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPackageBuilds — компиляционный смоук: cmd/gotcha-agent держит планку
// щадящей CMD-группы покрытия (T16), реальную логику проверяют тесты
// internal/agent. main() либо блокируется в agent.Run, либо делает os.Exit —
// гонять его через exec.Command здесь excessive, достаточно факта сборки.
func TestPackageBuilds(t *testing.T) {}

// TestCheckSubcommand компилирует настоящий бинарь и гоняет "--check" —
// именно эту команду install.sh вызывает через systemd-run ДО systemctl
// enable (ops-H2, install.sh). Код выхода обязан быть 0 на валидном
// конфиге и 2 на битом (тот же код, что и обычный запуск на ошибке
// конфига — от него зависит RestartPreventExitStatus=2 в юните), без
// обращения к сети: --check не должен запускать цикл сбора агента.
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
