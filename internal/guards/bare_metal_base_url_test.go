package guards

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/baseurl"
)

// Набор строк дублирует scripts/test-install-bare-metal.sh: новый плохой кейс — в оба места.
func TestBareMetalBaseURLValidatorIsSubsetOfNormalize(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash недоступен в PATH")
	}
	tree := Load(t)
	installer := filepath.Join(tree.Root, "internal", "docs", "install-bare-metal.sh")

	cases := []struct {
		in       string
		scriptOK bool
	}{
		{"https://gotcha.example.com", true},
		{"https://gotcha.example.com/", true},
		{"https://gotcha.example.com///", true},
		{"http://10.0.0.5:8080", true},
		{"http://[::1]:8080", true},
		{"https://gw.example.com/gotcha", true},
		{"https://gw.example.com/gotcha/", true},
		{"https://x/a%20b", true},
		{"HTTPS://gotcha.example.com", false},
		{"ftp://gotcha.example.com", false},
		{"https://", false},
		{"https:///path", false},
		{"gotcha.example.com", false},
		{"https://x?a=1", false},
		{"https://x#f", false},
		{"https://x y", false},
		{`https://x"`, false},
		{`https://x\`, false},
		{"https://x\nhttps://y", false},
		{"https://x&y", false},
		{"https://a%zz", false},
		{"https://user@x", false},
		{"http://[::1", false},
		{"https://x/a?a=1", false},
		{"https://x/a#f", false},
		{"https://x/a b", false},
		{`https://x/a"`, false},
	}
	for _, c := range cases {
		cmd := exec.Command("bash", "-c", `. "$1" && validate_base_url "$2"`, "validate", installer, c.in)
		out, err := cmd.Output()
		scriptOK := err == nil
		got := strings.TrimSuffix(string(out), "\n")
		if scriptOK != c.scriptOK {
			t.Errorf("validate_base_url(%q): принят=%v, ожидалось %v", c.in, scriptOK, c.scriptOK)
		}
		if !scriptOK {
			continue
		}
		want, nerr := baseurl.Normalize("GOTCHA_BASE_URL", c.in)
		if nerr != nil {
			t.Errorf("скрипт пропускает %q, а baseurl.Normalize отвергает: %v", c.in, nerr)
			continue
		}
		if got != want {
			t.Errorf("нормализация разошлась для %q: скрипт %q, приложение %q", c.in, got, want)
		}
	}
}
