package guards

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	renderUnitRe    = regexp.MustCompile(`(?s)render_unit\(\) \{\n.*?cat <<EOF\n(.*?)\nEOF\n`)
	docUnitBlockRe  = regexp.MustCompile(`(?s)cat >/etc/systemd/system/gotcha\.service <<'EOF'\n(.*?)\nEOF\n`)
	nightlyImageRe  = regexp.MustCompile(`(?m)^\s*image: \[(.+)\]\s*$`)
	distroClaimRe   = regexp.MustCompile(`(?m)^- \*\*(?:Linux-сервер|A Debian/Ubuntu-family Linux server)`)
	ubuntuRe        = regexp.MustCompile(`Ubuntu (\d+\.\d+)`)
	debianRe        = regexp.MustCompile(`Debian (\d+)\b`)
	manualTarballRe = regexp.MustCompile(`(?m)^TARBALL="([^"]+)"$`)
	manualFetchRe   = regexp.MustCompile(`(?m)^curl -fsSL -o "\$TARBALL" "\$URL/\$TARBALL"$`)
	manualSumRe     = regexp.MustCompile(`(?m)^grep " ([^"\\]+)\\\$" SHA256SUMS\.txt \| sha256sum -c -$`)
	manualExtractRe = regexp.MustCompile(`(?m)^tar xzf "\$TARBALL"$`)
	distURLRe       = regexp.MustCompile(`(?s)dist_url\(\) \{.*?printf '([^']*)\\n' (.*?)\n\}`)
)

func bareMetalDocPaths(root string) map[string]string {
	return map[string]string{
		"ru": filepath.Join(root, "internal", "docs", "ru", "installation-bare-metal.md"),
		"en": filepath.Join(root, "internal", "docs", "en", "installation-bare-metal.md"),
	}
}

func installerBody(t *testing.T, root string) string {
	t.Helper()
	return readDocFile(t, filepath.Join(root, "internal", "docs", "install-bare-metal.sh"))
}

// Копия юнита в доке ничем не держалась: удаление директив из render_unit оставляло
// зелёными все прогоны, а дока продолжала обещать их оператору.
func TestDocUnitMatchesRenderUnit(t *testing.T) {
	tree := Load(t)

	m := renderUnitRe.FindStringSubmatch(installerBody(t, tree.Root))
	if m == nil {
		t.Fatalf("install-bare-metal.sh: тело render_unit не найдено — сторож смотрит мимо функции")
	}
	want := strings.ReplaceAll(m[1], "MemoryMax=$memory_max", "MemoryMax=1024M")
	want = strings.ReplaceAll(want, "postgresql-$PG_MAJOR.service", "postgresql-17.service")
	if strings.Contains(want, "$") {
		t.Fatalf("render_unit: в эталоне осталась неразобранная подстановка:\n%s", want)
	}

	for locale, path := range bareMetalDocPaths(tree.Root) {
		doc := docUnitBlockRe.FindStringSubmatch(readDocFile(t, path))
		if doc == nil {
			t.Fatalf("%s: код-блок юнита не найден — сторож смотрит мимо страницы", locale)
		}
		if doc[1] != want {
			t.Errorf("%s: копия юнита в доке разошлась с render_unit 1024M:\n--- дока ---\n%s\n--- render_unit ---\n%s",
				locale, doc[1], want)
		}
	}
}

// "Заявляем ровно то, что гоняем": список дистрибутивов на странице обязан совпадать
// с матрицей ночного прогона, а не быть шире неё.
func TestBareMetalDocClaimsOnlyTestedDistros(t *testing.T) {
	tree := Load(t)

	workflow := readDocFile(t, filepath.Join(tree.Root, ".github", "workflows", "bare-metal-nightly.yml"))
	m := nightlyImageRe.FindStringSubmatch(workflow)
	if m == nil {
		t.Fatalf("bare-metal-nightly.yml: матрица образов не найдена — сторож смотрит мимо файла")
	}
	want := map[string]bool{}
	for _, image := range strings.Split(m[1], ",") {
		parts := strings.SplitN(strings.TrimSpace(image), ":", 2)
		if len(parts) != 2 {
			t.Fatalf("bare-metal-nightly.yml: образ %q не вида distro:version", image)
		}
		want[titleWord(parts[0])+" "+parts[1]] = true
	}
	if len(want) == 0 {
		t.Fatalf("bare-metal-nightly.yml: пустая матрица образов — сравнение ниже ничего бы не значило")
	}

	for locale, path := range bareMetalDocPaths(tree.Root) {
		claim := ""
		for _, line := range strings.Split(readDocFile(t, path), "\n") {
			if distroClaimRe.MatchString(line) {
				claim = line
				break
			}
		}
		if claim == "" {
			t.Fatalf("%s: строка с перечнем дистрибутивов не найдена — сторож смотрит мимо страницы", locale)
		}

		got := map[string]bool{}
		for _, v := range ubuntuRe.FindAllStringSubmatch(claim, -1) {
			got["Ubuntu "+v[1]] = true
		}
		for _, v := range debianRe.FindAllStringSubmatch(claim, -1) {
			got["Debian "+v[1]] = true
		}
		if !sameStringSet(got, want) {
			t.Errorf("%s: страница заявляет %v, ночная матрица гоняет %v", locale, sortedKeys(got), sortedKeys(want))
		}
	}
}

// Образец берётся из dist_url — единственного места, знающего форму имени ассета:
// литерал рядом с ним сверял бы копию с копией, а не с истиной.
func releaseTarballName(t *testing.T, installer string) string {
	t.Helper()
	m := distURLRe.FindStringSubmatch(installer)
	if m == nil {
		t.Fatalf("install-bare-metal.sh: printf в dist_url не разобран — сторож смотрит мимо функции")
	}
	placeholder := map[string]string{
		`"${base%/}"`: "BASE",
		`"$version"`:  "X.Y.Z",
		`"$arch"`:     "<arch>",
	}
	url := m[1]
	for _, arg := range strings.Fields(m[2]) {
		value, known := placeholder[arg]
		if !known {
			t.Fatalf("dist_url: незнакомый аргумент printf %q — образец имени собрался бы неверно", arg)
		}
		i := strings.Index(url, "%s")
		if i < 0 {
			t.Fatalf("dist_url: аргументов printf больше, чем %%s в формате %q", m[1])
		}
		url = url[:i] + value + url[i+2:]
	}
	if strings.Contains(url, "%s") {
		t.Fatalf("dist_url: в формате %q остались %%s без аргументов", m[1])
	}
	return url[strings.LastIndex(url, "/")+1:]
}

// Тарбол сохранялся под именем gotcha.tar.gz, а SHA256SUMS.txt называет его полным
// именем релиза: `sha256sum -c` не находил ни одной строки и падал всегда.
func TestManualInstallChecksumNamesMatch(t *testing.T) {
	tree := Load(t)
	want := releaseTarballName(t, installerBody(t, tree.Root))

	for locale, path := range bareMetalDocPaths(tree.Root) {
		body := readDocFile(t, path)
		name := manualTarballRe.FindStringSubmatch(body)
		if name == nil {
			t.Fatalf("%s: имя тарбола в ручном пути не найдено — сторож смотрит мимо страницы", locale)
		}
		if name[1] != want {
			t.Errorf("%s: имя тарбола %q, а dist_url печатает %q", locale, name[1], want)
		}
		if !manualFetchRe.MatchString(body) {
			t.Errorf("%s: тарбол качается не под тем же именем, под которым сохраняется", locale)
		}
		sum := manualSumRe.FindStringSubmatch(body)
		if sum == nil {
			t.Fatalf("%s: сверка суммы в ручном пути не найдена — сторож смотрит мимо страницы", locale)
		}
		if sum[1] != "$TARBALL" {
			t.Errorf("%s: sha256sum -c ищет в SHA256SUMS.txt строку %q, а не имя скачанного файла", locale, sum[1])
		}
		if !manualExtractRe.MatchString(body) {
			t.Errorf("%s: распаковывается не тот файл, который скачан и сверен", locale)
		}
	}
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func sameStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Постинстал clickhouse-server на живом терминале спрашивает пароль пользователя
// default, и заданный там пароль ломает создание базы следующим шагом. Скрипт
// ставит пакеты неинтерактивно — ручной путь в доке обязан делать то же.
func TestBareMetalAptInstallsAreNonInteractive(t *testing.T) {
	tree := Load(t)

	sources := bareMetalDocPaths(tree.Root)
	sources["installer"] = filepath.Join(tree.Root, "internal", "docs", "install-bare-metal.sh")

	for name, path := range sources {
		seen := 0
		for _, line := range strings.Split(readDocFile(t, path), "\n") {
			if !strings.Contains(line, "apt-get install") {
				continue
			}
			seen++
			if !strings.HasPrefix(strings.TrimSpace(line), "DEBIAN_FRONTEND=noninteractive apt-get install") {
				t.Errorf("%s: apt-get install без DEBIAN_FRONTEND=noninteractive: %q", name, strings.TrimSpace(line))
			}
		}
		if seen == 0 {
			t.Errorf("%s: ни одной установки пакетов не найдено — сторож смотрит мимо файла", name)
		}
	}
}
