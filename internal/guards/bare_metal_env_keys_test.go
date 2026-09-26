package guards

import (
	"regexp"
	"sort"
	"testing"
)

var (
	renderEnvFileRe       = regexp.MustCompile(`(?s)render_env_file\(\) \{\n.*?cat <<EOF\n(.*?)\nEOF\n`)
	docEnvFileBlockRe     = regexp.MustCompile(`(?s)cat >/etc/gotcha/gotcha\.env <<EOF\n(.*?)\nEOF\n`)
	envLineRe             = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=(.*)$`)
	trustedProxiesConstRe = regexp.MustCompile(`(?m)^TRUSTED_PROXIES_LOOPBACK="([^"]+)"$`)
)

func envBlockKeys(block string) map[string]string {
	keys := map[string]string{}
	for _, m := range envLineRe.FindAllStringSubmatch(block, -1) {
		keys[m[1]] = m[2]
	}
	return keys
}

// Ручной путь, записавший env без ключа, который пишет скрипт, расходится с ним молча.
func TestBareMetalDocEnvFileMatchesRenderEnvFile(t *testing.T) {
	tree := Load(t)
	installer := installerBody(t, tree.Root)
	m := renderEnvFileRe.FindStringSubmatch(installer)
	if m == nil {
		t.Fatalf("install-bare-metal.sh: тело render_env_file не найдено — сторож смотрит мимо функции")
	}
	want := envBlockKeys(m[1])
	if len(want) == 0 {
		t.Fatalf("install-bare-metal.sh: render_env_file без ключей — сторож смотрит мимо функции")
	}
	c := trustedProxiesConstRe.FindStringSubmatch(installer)
	if c == nil {
		t.Fatalf("install-bare-metal.sh: константа TRUSTED_PROXIES_LOOPBACK не найдена")
	}

	for locale, path := range bareMetalDocPaths(tree.Root) {
		d := docEnvFileBlockRe.FindStringSubmatch(readDocFile(t, path))
		if d == nil {
			t.Fatalf("%s: блок cat >/etc/gotcha/gotcha.env не найден — сторож смотрит мимо страницы", locale)
		}
		got := envBlockKeys(d[1])
		var missing, extra []string
		for k := range want {
			if _, ok := got[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: ключи env шага 6 разошлись с render_env_file: нет %v, лишние %v", locale, missing, extra)
		}
		if got["GOTCHA_TRUSTED_PROXIES"] != c[1] {
			t.Errorf("%s: GOTCHA_TRUSTED_PROXIES=%q в доке, скрипт пишет %q", locale, got["GOTCHA_TRUSTED_PROXIES"], c[1])
		}
	}
}
