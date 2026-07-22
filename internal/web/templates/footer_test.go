package templates

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

// Подвал живёт в layoutBody, поэтому проверяем через любую страницу app-shell.
func TestLayoutFooterShowsVersion(t *testing.T) {
	out := renderTo(t, DocsIndex(nil, "u@e.com"))
	if !strings.Contains(out, version.Version()) {
		t.Fatalf("подвал app-shell должен содержать версию %q", version.Version())
	}
	if !strings.Contains(out, "app-footer") {
		t.Fatal("ждали контейнер .app-footer")
	}
}
