package guards

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Что сверяет сторож: пакеты, которые preflight установщика сам доставляет (PKG_HINTS для
// команд из required_commands), обязаны стоять в строке «Подготовка хоста» ручной
// установки - для каждого семейства (debian/rhel) и в обеих локалях; RU и EN строки
// одного семейства обязаны совпадать по составу.
//
// Вне охвата, осознанно:
//   - rpm и dnf на RHEL: это сам менеджер пакетов, preflight падает на них без
//     автодоставки (case rpm | dnf в preflight_prerequisites), ставить их строкой
//     dnf install нечем, в строку они не входят.
//   - runuser при --skip-databases: в required_commands он условный, но без флага
//     обязателен, а строка доки описывает обычный путь, поэтому сверяется.
//   - Пакеты, которых нет в PKG_HINTS (postgresql, nginx и т.д.): это другие шаги доки.
//
// Историческая причина: 2026-09-22 дока обещала runuser "из коробки" на RHEL 10,
// а установщик там падал с кодом 3 - строка dnf install не содержала util-linux.
var (
	pkgHintsDeclRe    = regexp.MustCompile(`(?s)declare -gA PKG_HINTS=\((.*?)\)`)
	pkgHintsPairRe    = regexp.MustCompile(`\[([a-z0-9_]+)\]=([A-Za-z0-9._+-]+)`)
	pkgHintsAssignRe  = regexp.MustCompile(`(?m)^\s*PKG_HINTS\[([a-z0-9_]+)\]=([A-Za-z0-9._+-]+)\s*$`)
	platformPathsRe   = regexp.MustCompile(`(?s)apply_platform_paths\(\) \{\n(.*?)\n\}\n`)
	rhelBranchRe      = regexp.MustCompile(`(?s)if \[ "\$HOST_FAMILY" = rhel \]; then\n(.*?)\n\s*return 0\n\s*fi\n(.*)$`)
	requiredCmdsRe    = regexp.MustCompile(`(?s)required_commands\(\) \{\n(.*?)\n\}\n`)
	requiredForRe     = regexp.MustCompile(`for cmd in ([a-z0-9_ ]+); do`)
	requiredPrintfRe  = regexp.MustCompile(`printf '([a-z0-9_]+)\\n'`)
	requiredRhelRe    = regexp.MustCompile(`(?s)if \[ "\$family" = rhel \]; then\n(.*?)\n\s*fi\n`)
	docAptPrepRe      = regexp.MustCompile(`(?m)^(?:DEBIAN_FRONTEND=noninteractive )?apt-get install -y ([^\\\n]+)$`)
	docDnfPrepRe      = regexp.MustCompile(`(?m)^dnf install -y ([^\\\n]+)$`)
	lineContinuedRe   = regexp.MustCompile(`\\\n[ \t]*`)
	pkgManagerCmdsSet = map[string]bool{"rpm": true, "dnf": true}
)

// family -> команда -> пакет.
func parsePkgHints(t *testing.T, installer string) map[string]map[string]string {
	t.Helper()
	body := platformPathsRe.FindStringSubmatch(installer)
	if body == nil {
		t.Fatalf("install-bare-metal.sh: тело apply_platform_paths не найдено - сторож смотрит мимо функции")
	}
	decl := pkgHintsDeclRe.FindStringSubmatch(body[1])
	if decl == nil {
		t.Fatalf("install-bare-metal.sh: блок declare -gA PKG_HINTS не найден - сторож смотрит мимо карты")
	}
	branch := rhelBranchRe.FindStringSubmatch(body[1])
	if branch == nil {
		t.Fatalf("install-bare-metal.sh: ветка rhel в apply_platform_paths не найдена - сторож смотрит мимо семейств")
	}
	out := map[string]map[string]string{"debian": {}, "rhel": {}}
	for _, m := range pkgHintsPairRe.FindAllStringSubmatch(decl[1], -1) {
		out["debian"][m[1]] = m[2]
		out["rhel"][m[1]] = m[2]
	}
	if len(out["debian"]) == 0 {
		t.Fatalf("install-bare-metal.sh: PKG_HINTS по умолчанию пуст - сторож смотрит мимо карты")
	}
	for _, m := range pkgHintsAssignRe.FindAllStringSubmatch(branch[1], -1) {
		out["rhel"][m[1]] = m[2]
	}
	for _, m := range pkgHintsAssignRe.FindAllStringSubmatch(branch[2], -1) {
		out["debian"][m[1]] = m[2]
	}
	return out
}

// family -> команды, которые required_commands требует без флагов пропуска.
func parseRequiredCommands(t *testing.T, installer string) map[string][]string {
	t.Helper()
	body := requiredCmdsRe.FindStringSubmatch(installer)
	if body == nil {
		t.Fatalf("install-bare-metal.sh: тело required_commands не найдено - сторож смотрит мимо функции")
	}
	forList := requiredForRe.FindStringSubmatch(body[1])
	if forList == nil {
		t.Fatalf("install-bare-metal.sh: базовый список for cmd in ... в required_commands не найден")
	}
	base := strings.Fields(forList[1])
	rhelBlock := requiredRhelRe.FindStringSubmatch(body[1])
	if rhelBlock == nil {
		t.Fatalf("install-bare-metal.sh: блок rhel в required_commands не найден")
	}
	var rhelOnly []string
	for _, m := range requiredPrintfRe.FindAllStringSubmatch(rhelBlock[1], -1) {
		rhelOnly = append(rhelOnly, m[1])
	}
	rest := strings.Replace(body[1], rhelBlock[0], "", 1)
	var common []string
	for _, m := range requiredPrintfRe.FindAllStringSubmatch(rest, -1) {
		common = append(common, m[1])
	}
	debian := append(append([]string{}, base...), common...)
	rhel := append(append(append([]string{}, base...), rhelOnly...), common...)
	return map[string][]string{"debian": debian, "rhel": rhel}
}

// locale -> family -> пакеты строки подготовки хоста (первая строка apt-get/dnf install -y).
func parseHostPrepLines(t *testing.T, docs map[string]string) map[string]map[string][]string {
	t.Helper()
	out := map[string]map[string][]string{}
	for locale, doc := range docs {
		doc = lineContinuedRe.ReplaceAllString(doc, " ")
		out[locale] = map[string][]string{}
		for family, re := range map[string]*regexp.Regexp{"debian": docAptPrepRe, "rhel": docDnfPrepRe} {
			m := re.FindStringSubmatch(doc)
			if m == nil {
				t.Fatalf("%s: строка подготовки хоста (%s install -y ...) не найдена - сторож смотрит мимо шага 1", locale, family)
			}
			out[locale][family] = strings.Fields(m[1])
		}
	}
	return out
}

func checkHostPrepPackages(hints map[string]map[string]string, required map[string][]string, prep map[string]map[string][]string) []string {
	var problems []string
	locales := make([]string, 0, len(prep))
	for l := range prep {
		locales = append(locales, l)
	}
	sort.Strings(locales)
	for _, family := range []string{"debian", "rhel"} {
		for _, locale := range locales {
			have := map[string]bool{}
			for _, p := range prep[locale][family] {
				have[p] = true
			}
			for _, cmd := range required[family] {
				if pkgManagerCmdsSet[cmd] {
					continue
				}
				pkg, ok := hints[family][cmd]
				if !ok {
					problems = append(problems, fmt.Sprintf("семейство %s: команда %q требуется preflight, но её нет в PKG_HINTS установщика", family, cmd))
					continue
				}
				if !have[pkg] {
					problems = append(problems, fmt.Sprintf(
						"семейство %s, локаль %s: в строке подготовки хоста нет пакета %q (команда %q, PKG_HINTS[%s]=%s) - добавьте его в строку доки или поправьте PKG_HINTS",
						family, locale, pkg, cmd, cmd, pkg))
				}
			}
		}
		if len(locales) < 2 {
			continue
		}
		base := locales[0]
		for _, other := range locales[1:] {
			a, b := setOf(prep[base][family]), setOf(prep[other][family])
			for p := range a {
				if !b[p] {
					problems = append(problems, fmt.Sprintf("семейство %s: пакет %q есть в строке %s, но нет в строке %s", family, p, base, other))
				}
			}
			for p := range b {
				if !a[p] {
					problems = append(problems, fmt.Sprintf("семейство %s: пакет %q есть в строке %s, но нет в строке %s", family, p, other, base))
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func setOf(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

func TestCheckHostPrepPackagesDetectsDrift(t *testing.T) {
	hints := map[string]map[string]string{
		"debian": {"curl": "curl", "runuser": "util-linux", "ss": "iproute2"},
		"rhel":   {"curl": "curl", "runuser": "util-linux", "ss": "iproute", "rpm": "rpm", "dnf": "dnf"},
	}
	required := map[string][]string{
		"debian": {"curl", "ss", "runuser"},
		"rhel":   {"curl", "ss", "rpm", "dnf", "runuser"},
	}
	good := map[string]map[string][]string{
		"ru": {"debian": {"curl", "iproute2", "util-linux"}, "rhel": {"curl", "iproute", "util-linux"}},
		"en": {"debian": {"curl", "iproute2", "util-linux"}, "rhel": {"curl", "iproute", "util-linux"}},
	}
	cases := []struct {
		name   string
		mutate func(p map[string]map[string][]string)
		want   string
	}{
		{"полная строка, rpm и dnf вне охвата", func(p map[string]map[string][]string) {}, ""},
		{"нет util-linux в RU rhel", func(p map[string]map[string][]string) {
			p["ru"]["rhel"] = []string{"curl", "iproute"}
		}, `семейство rhel, локаль ru: в строке подготовки хоста нет пакета "util-linux" (команда "runuser"`},
		{"RU и EN расходятся", func(p map[string]map[string][]string) {
			p["en"]["debian"] = []string{"curl", "iproute2", "util-linux", "extra"}
		}, `семейство debian: пакет "extra" есть в строке en, но нет в строке ru`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prep := map[string]map[string][]string{}
			for l, fams := range good {
				prep[l] = map[string][]string{}
				for f, pk := range fams {
					prep[l][f] = append([]string{}, pk...)
				}
			}
			c.mutate(prep)
			problems := strings.Join(checkHostPrepPackages(hints, required, prep), "\n")
			if c.want == "" && problems != "" {
				t.Fatalf("ложное срабатывание:\n%s", problems)
			}
			if c.want != "" && !strings.Contains(problems, c.want) {
				t.Fatalf("нарушение не найдено, ожидали фрагмент %q, получили:\n%s", c.want, problems)
			}
		})
	}
}

func TestBareMetalDocHostPrepPackagesMatchPkgHints(t *testing.T) {
	tree := Load(t)
	installer := installerBody(t, tree.Root)
	docs := map[string]string{}
	for locale, path := range bareMetalDocPaths(tree.Root) {
		docs[locale] = readDocFile(t, path)
	}
	problems := checkHostPrepPackages(parsePkgHints(t, installer), parseRequiredCommands(t, installer), parseHostPrepLines(t, docs))
	for _, p := range problems {
		t.Error(p)
	}
}

// Строка подготовки хоста длиннее 100 символов переносится через "\" (предел
// docs_pdf_width_test), и парсер обязан склеить её, а не взять следующую
// apt-get/dnf install из другого шага доки.
func TestParseHostPrepLinesJoinsContinuations(t *testing.T) {
	doc := "```bash\nDEBIAN_FRONTEND=noninteractive apt-get install -y \\\n    curl tar util-linux\n```\n\n" +
		"```bash\napt-get install -y nginx\n```\n\n" +
		"```bash\ndnf install -y curl \\\n  util-linux\n```\n"
	got := parseHostPrepLines(t, map[string]string{"ru": doc})
	if s := strings.Join(got["ru"]["debian"], " "); s != "curl tar util-linux" {
		t.Errorf("debian: получили %q, ожидали склеенную строку %q", s, "curl tar util-linux")
	}
	if s := strings.Join(got["ru"]["rhel"], " "); s != "curl util-linux" {
		t.Errorf("rhel: получили %q, ожидали склеенную строку %q", s, "curl util-linux")
	}
}
