package guards

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var (
	renderUnitRe     = regexp.MustCompile(`(?s)render_unit\(\) \{\n.*?cat <<EOF\n(.*?)\nEOF\n`)
	docUnitBlockRe   = regexp.MustCompile(`(?s)cat >/etc/systemd/system/gotcha\.service <<'EOF'\n(.*?)\nEOF\n`)
	osTableHeaderRe  = regexp.MustCompile(`(?m)^\|\s*(?:ОС|OS)\s*\|\s*(?:Архитектуры|Architectures)\s*\|\s*$`)
	osTableRowRe     = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|\s*$`)
	manualTarballRe  = regexp.MustCompile(`(?m)^TARBALL="([^"]+)"$`)
	manualFetchRe    = regexp.MustCompile(`(?m)^curl -fsSL -o "\$TARBALL" "\$URL/\$TARBALL"$`)
	manualSumRe      = regexp.MustCompile(`(?m)^grep " ([^"\\]+)\\\$" SHA256SUMS\.txt \| sha256sum -c -$`)
	manualExtractRe  = regexp.MustCompile(`(?m)^tar xzf "\$TARBALL"$`)
	distURLRe        = regexp.MustCompile(`(?s)dist_url\(\) \{.*?printf '([^']*)\\n' (.*?)\n\}`)
	shellSeparatorRe = regexp.MustCompile(`&&|\|\||;|\|`)
	dnfInvocationRe  = regexp.MustCompile(`\bdnf\s+\S`)
	quotedStringRe   = regexp.MustCompile(`'[^']*'|"[^"]*"`)
)

// Единственное место, где решение «этот образ бинарно совместим с RHEL» видно в
// коде, который его проверяет, а не только в covers workflow'а.
var coversAliases = map[string][]string{
	"AlmaLinux 9":    {"RHEL 9"},
	"AlmaLinux 10":   {"RHEL 10"},
	"Rocky Linux 9":  {"RHEL 9"},
	"Rocky Linux 10": {"RHEL 10"},
}

var imageDistroNames = map[string]string{
	"almalinux":  "AlmaLinux",
	"rockylinux": "Rocky Linux",
	"debian":     "Debian",
	"ubuntu":     "Ubuntu",
}

// Шаг, где реально исполняется e2e-ассерт: если здесь есть условие if,
// ассерт может не выполниться и всё равно засчитаться (cleanup-шаги — законно).
const assertingStepPrefix = "install-bare-metal.sh "

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

type nightlyMatrixEntry struct {
	Image  string `yaml:"image"`
	Arch   string `yaml:"arch"`
	Runner string `yaml:"runner"`
	Covers string `yaml:"covers"`
}

type nightlyStep struct {
	Name            string `yaml:"name"`
	ContinueOnError bool   `yaml:"continue-on-error"`
	If              string `yaml:"if"`
}

type nightlyJob struct {
	ContinueOnError bool   `yaml:"continue-on-error"`
	If              string `yaml:"if"`
	Strategy        struct {
		Matrix struct {
			Include []nightlyMatrixEntry `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []nightlyStep `yaml:"steps"`
}

type nightlyWorkflow struct {
	Jobs map[string]nightlyJob `yaml:"jobs"`
}

func loadNightlyWorkflow(t *testing.T, root string) nightlyWorkflow {
	t.Helper()
	raw := readDocFile(t, filepath.Join(root, ".github", "workflows", "bare-metal-nightly.yml"))
	var wf nightlyWorkflow
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("bare-metal-nightly.yml: разбор YAML: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("bare-metal-nightly.yml: job'ы не найдены — сторож смотрит мимо файла")
	}
	return wf
}

func sortedJobNames(jobs map[string]nightlyJob) []string {
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func splitCovers(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// Образ вида "rockylinux/rockylinux:10" или "almalinux:9" — путь до двоеточия
// может нести неймспейс, значение после него — версия дистрибутива.
func osFromImage(t *testing.T, image string) string {
	t.Helper()
	name, version, ok := strings.Cut(image, ":")
	if !ok {
		t.Fatalf("bare-metal-nightly.yml: образ %q не вида distro:version", image)
	}
	name = name[strings.LastIndex(name, "/")+1:]
	distro, known := imageDistroNames[name]
	if !known {
		t.Fatalf("bare-metal-nightly.yml: образ %q — незнакомый дистрибутив %q, добавь его в imageDistroNames", image, name)
	}
	return distro + " " + version
}

func allowedCovers(self string) map[string]bool {
	allowed := map[string]bool{self: true}
	for _, alias := range coversAliases[self] {
		allowed[alias] = true
	}
	return allowed
}

func archSetsEqual(a, b map[string]map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for os, arches := range a {
		other, ok := b[os]
		if !ok || len(arches) != len(other) {
			return false
		}
		for arch := range arches {
			if !other[arch] {
				return false
			}
		}
	}
	return true
}

func describeArchSets(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for os, arches := range m {
		out = append(out, os+": "+strings.Join(sortedKeys(arches), ", "))
	}
	sort.Strings(out)
	return out
}

func parseOSTable(t *testing.T, locale, doc string) map[string]map[string]bool {
	t.Helper()
	loc := osTableHeaderRe.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("%s: таблица ОС (заголовок «ОС | Архитектуры») не найдена — сторож смотрит мимо страницы", locale)
	}
	lines := strings.Split(doc[loc[1]:], "\n")
	if len(lines) < 3 {
		t.Fatalf("%s: за заголовком таблицы ОС нет строки разделителя и данных", locale)
	}
	result := map[string]map[string]bool{}
	for _, line := range lines[2:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		m := osTableRowRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: строка таблицы ОС не разобрана: %q", locale, line)
		}
		os := strings.TrimSpace(m[1])
		arches := map[string]bool{}
		for _, arch := range strings.Split(m[2], ",") {
			arch = strings.TrimSpace(arch)
			if arch != "" {
				arches[arch] = true
			}
		}
		if len(arches) == 0 {
			t.Fatalf("%s: строка таблицы ОС %q — пустой список архитектур", locale, os)
		}
		result[os] = arches
	}
	if len(result) == 0 {
		t.Fatalf("%s: таблица ОС не содержит ни одной строки — сторож смотрит мимо страницы", locale)
	}
	return result
}

// "Заявляем ровно то, что гоняем": таблица ОС на странице обязана совпадать с
// объединением обоих job'ов ночной матрицы, а не быть шире или уже него.
func TestBareMetalDocClaimsOnlyTestedDistros(t *testing.T) {
	tree := Load(t)
	wf := loadNightlyWorkflow(t, tree.Root)

	union := map[string]map[string]bool{}
	for _, jobName := range sortedJobNames(wf.Jobs) {
		job := wf.Jobs[jobName]
		if job.ContinueOnError {
			t.Errorf("bare-metal-nightly.yml: job %q несёт continue-on-error: true — незавершившийся прогон засчитался бы подтверждающим", jobName)
		}
		if job.If != "" {
			t.Errorf("bare-metal-nightly.yml: job %q несёт условие if: %q на уровне job — прогон может не выполниться и всё равно засчитаться", jobName, job.If)
		}
		assertingSeen := 0
		for _, step := range job.Steps {
			if step.ContinueOnError {
				t.Errorf("bare-metal-nightly.yml: job %q шаг %q несёт continue-on-error: true — падение шага можно замаскировать", jobName, step.Name)
			}
			if !strings.HasPrefix(step.Name, assertingStepPrefix) {
				continue
			}
			assertingSeen++
			if step.If != "" {
				t.Errorf("bare-metal-nightly.yml: job %q шаг %q несёт условие if: %q — ассерт может не выполниться и всё равно засчитаться", jobName, step.Name, step.If)
			}
		}
		if assertingSeen == 0 {
			t.Errorf("bare-metal-nightly.yml: job %q — ни одного шага %q* не найдено, сторож смотрит мимо файла", jobName, assertingStepPrefix)
		}
		entries := job.Strategy.Matrix.Include
		if len(entries) == 0 {
			t.Fatalf("bare-metal-nightly.yml: job %q — пустая матрица include, сторож смотрит мимо файла", jobName)
		}
		for i, entry := range entries {
			covers := splitCovers(entry.Covers)
			if len(covers) == 0 {
				t.Errorf("bare-metal-nightly.yml: job %q запись %d — пустой covers, неясно, что подтверждает прогон", jobName, i)
				continue
			}
			if entry.Image != "" {
				self := osFromImage(t, entry.Image)
				allowed := allowedCovers(self)
				if !covers[self] {
					t.Errorf("bare-metal-nightly.yml: job %q запись %d (%s) — covers %v не содержит %q, выведенное из image",
						jobName, i, entry.Image, sortedKeys(covers), self)
				}
				for name := range covers {
					if !allowed[name] {
						t.Errorf("bare-metal-nightly.yml: job %q запись %d (%s) — covers заявляет %q, это не сам образ и не разрешённый псевдоним из coversAliases",
							jobName, i, entry.Image, name)
					}
				}
			}
			if entry.Arch == "" {
				t.Errorf("bare-metal-nightly.yml: job %q запись %d — пустой arch", jobName, i)
				continue
			}
			for name := range covers {
				if union[name] == nil {
					union[name] = map[string]bool{}
				}
				union[name][entry.Arch] = true
			}
		}
	}

	for locale, path := range bareMetalDocPaths(tree.Root) {
		table := parseOSTable(t, locale, readDocFile(t, path))
		if !archSetsEqual(table, union) {
			t.Errorf("%s: таблица ОС заявляет %v, ночная матрица гоняет %v",
				locale, describeArchSets(table), describeArchSets(union))
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// -y — глобальный флаг, безвредный и на read-only подкомандах: repolist на EL
// сам требует его при первом обращении к репозиторию, исключений нет.
func hasShortYFlag(fields []string) bool {
	for _, f := range fields {
		if strings.HasPrefix(f, "--") {
			continue
		}
		if strings.HasPrefix(f, "-") && strings.ContainsRune(f[1:], 'y') {
			return true
		}
	}
	return false
}

func dnfSubcommand(fields []string) string {
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		return f
	}
	return ""
}

// Постинстал clickhouse-server на живом терминале спрашивает пароль пользователя
// default — скрипт ставит пакеты неинтерактивно, ручной путь в доке обязан то же.
func TestBareMetalAptInstallsAreNonInteractive(t *testing.T) {
	tree := Load(t)

	sources := bareMetalDocPaths(tree.Root)
	sources["installer"] = filepath.Join(tree.Root, "internal", "docs", "install-bare-metal.sh")

	for name, path := range sources {
		aptSeen := 0
		dnfSeen := 0
		for _, line := range strings.Split(readDocFile(t, path), "\n") {
			if strings.Contains(line, "apt-get install") {
				aptSeen++
				if !strings.HasPrefix(strings.TrimSpace(line), "DEBIAN_FRONTEND=noninteractive apt-get install") {
					t.Errorf("%s: apt-get install без DEBIAN_FRONTEND=noninteractive: %q", name, strings.TrimSpace(line))
				}
			}
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			// Советы оператору (printf/echo) кавычатся целиком, одинарно или
			// двойно — без вырезания их dnf-упоминания читались бы как вызовы.
			unquoted := quotedStringRe.ReplaceAllString(line, "")
			for _, segment := range shellSeparatorRe.Split(unquoted, -1) {
				loc := dnfInvocationRe.FindStringIndex(segment)
				if loc == nil {
					continue
				}
				dnfSeen++
				fields := strings.Fields(segment[loc[0]:])
				if !hasShortYFlag(fields) {
					t.Errorf("%s: dnf %s без флага -y: %q", name, dnfSubcommand(fields), strings.TrimSpace(segment))
				}
			}
		}
		if aptSeen == 0 {
			t.Errorf("%s: ни одной установки пакетов apt-get не найдено — сторож смотрит мимо файла", name)
		}
		// Дока ещё не несёт EL-ветку ручного пути (задача 9), dnf там пока нет;
		// в инсталляторе dnf уже есть, и отсутствие строк там — сторож ослеп.
		if name == "installer" && dnfSeen < 3 {
			t.Errorf("%s: строк с dnf найдено %d (< 3) — сторож смотрит мимо файла", name, dnfSeen)
		}
	}
}
