package guards

import (
	"regexp"
	"sort"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

// Вызовы с переменными (сама реализация в help.templ) не матчатся — у них
// вторая группа не литерал.
var helpGuideCall = regexp.MustCompile(`helpPanel(?:With)?\("([^"]+)",\s*"([^"]+)"\)`)

func TestHelpPanelGuideSlugsExist(t *testing.T) {
	tree := Load(t)

	slugs := map[string][]string{} // slug -> области, которые на него ссылаются
	for _, f := range tree.Templates {
		for _, m := range helpGuideCall.FindAllStringSubmatch(f.Body, -1) {
			slugs[m[2]] = append(slugs[m[2]], m[1])
		}
	}
	if len(slugs) < 10 {
		t.Fatalf("найдено %d гайдов панели помощи — сканер сломан", len(slugs))
	}

	ordered := make([]string, 0, len(slugs))
	for slug := range slugs {
		ordered = append(ordered, slug)
	}
	sort.Strings(ordered)

	for _, slug := range ordered {
		for _, loc := range []string{"ru", "en"} {
			if _, _, ok := docs.Render(loc, slug); !ok {
				sort.Strings(slugs[slug])
				t.Errorf("[%s] панель помощи разделов %v ведёт на /docs/%s, но такой страницы нет: добавьте internal/docs/%s/%s.md и запись в registry (internal/docs/docs.go)",
					loc, slugs[slug], slug, loc, slug)
			}
		}
	}
}
