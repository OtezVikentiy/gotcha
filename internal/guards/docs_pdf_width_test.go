package guards

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

var (
	pdfMaxLineRe = regexp.MustCompile(`(?m)^MAX_LINE_LEN = (\d+)$`)
	pdfBundleRe  = regexp.MustCompile(`(?m)^BUNDLE=\(([^)]+)\)$`)
	pdfFenceRe   = regexp.MustCompile("^\\s*```")
)

// Ширину строк в код-блоках держал только сборщик PDF, а он запускается на теге:
// перебор в 127 символов доехал до релиза и уронил сборку артефактов v1.7.1.
// Предел и состав комплекта читаются из самого scripts/docs-pdf.sh, чтобы сторож
// сверялся с истиной, а не с её копией.
func TestPDFBundleCodeBlockWidth(t *testing.T) {
	tree := Load(t)

	script := readDocFile(t, filepath.Join(tree.Root, "scripts", "docs-pdf.sh"))
	limitMatch := pdfMaxLineRe.FindStringSubmatch(script)
	if limitMatch == nil {
		t.Fatalf("scripts/docs-pdf.sh: MAX_LINE_LEN не найден — сторож смотрит мимо проверки")
	}
	limit, err := strconv.Atoi(limitMatch[1])
	if err != nil || limit <= 0 {
		t.Fatalf("scripts/docs-pdf.sh: MAX_LINE_LEN = %q не разобран", limitMatch[1])
	}
	bundleMatch := pdfBundleRe.FindStringSubmatch(script)
	if bundleMatch == nil {
		t.Fatalf("scripts/docs-pdf.sh: список BUNDLE не найден — сторож смотрит мимо комплекта")
	}
	pages := strings.Fields(bundleMatch[1])
	if len(pages) == 0 {
		t.Fatalf("scripts/docs-pdf.sh: BUNDLE пуст")
	}

	for _, locale := range []string{"ru", "en"} {
		for _, page := range pages {
			path := filepath.Join(tree.Root, "internal", "docs", locale, page+".md")
			inFence := false
			for i, line := range strings.Split(readDocFile(t, path), "\n") {
				if pdfFenceRe.MatchString(line) {
					inFence = !inFence
					continue
				}
				if !inFence {
					continue
				}
				if n := utf8.RuneCountInString(line); n > limit {
					t.Errorf("%s/%s.md:%d: строка код-блока %d символов при пределе %d — pandoc её перенесёт, и в текстовом слое PDF символ потеряется или добавится: %q",
						locale, page, i+1, n, limit, line)
				}
			}
		}
	}
}
