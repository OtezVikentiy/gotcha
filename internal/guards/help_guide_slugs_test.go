package guards

import (
	"regexp"
	"sort"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

// helpGuideCall ловит оба вида панели «Что это за раздел?» с литеральными
// аргументами: helpPanel(area, guideSlug) и helpPanelWith(area, guideSlug).
// Вызовы с переменными (сама реализация в help.templ) не матчатся — у них
// вторая группа не литерал.
var helpGuideCall = regexp.MustCompile(`helpPanel(?:With)?\("([^"]+)",\s*"([^"]+)"\)`)

// TestHelpPanelGuideSlugsExist — вторая половина контракта панели помощи.
// TestHelpPanelKeysResolve (i18n_dynamic_test.go) проверяет ПЕРВЫЙ аргумент
// (что у области есть help.<area>.title/.body), а ссылка «Подробнее в
// документации» строится из ВТОРОГО и до сих пор не проверялась никем:
// guideSlug, которого нет в реестре internal/docs, даёт с живой страницы
// ссылку в 404, и ни один тест этого не видит — ни сторож ключей (другой
// аргумент), ни TestEveryMarkdownFileIsInRegistry (он сверяет реестр с
// диском, а не с шаблонами). Цена ошибки — тихая: панель выглядит рабочей,
// пока по ссылке не кликнут.
//
// Проверка идёт по обеим локалям через docs.Render, а не по списку slug'ов:
// страница обязана существовать и рендериться и на русском, и на английском,
// иначе EN-читатель упрётся в 404 там, где RU-читатель прочитал гайд.
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
