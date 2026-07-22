package templates

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

func TestAboutShowsBuildInfo(t *testing.T) {
	info := version.Info{Version: "v9.9.9", Commit: "deadbee", Date: "2026-07-22", Go: "go1.26"}
	out := renderTo(t, About(info, "u@e.com"))
	for _, want := range []string{"v9.9.9", "deadbee", "2026-07-22", "go1.26"} {
		if !strings.Contains(out, want) {
			t.Errorf("страница About должна содержать %q", want)
		}
	}
}
