package guards

import (
	"strings"
	"testing"
)

// minSameOriginBranches — нижняя граница: на момент написания в internal/web
// 60 веток if !sameOrigin. Меньше 55 найденных — обход ослеп (сузился до
// части файлов), а не код стал чище: ветки уходят только вместе со своими
// ручками, десятками за раз они не исчезают.
const minSameOriginBranches = 55

// TestSameOriginBranchesUseDenyCrossOrigin — каждая ветка if !sameOrigin в
// internal/web обязана отвечать через h.denyCrossOrigin(w, r): он даёт лог,
// метрику и страницу с объяснением разом. Голый http.Error("forbidden")
// возвращает находку №37 — 403 на регистрации при зелёном /readyz и пустом
// журнале; renderError мимо denyCrossOrigin теряет лог и метрику.
func TestSameOriginBranchesUseDenyCrossOrigin(t *testing.T) {
	tree := Load(t)
	branches := 0
	for _, f := range tree.GoFiles {
		if !strings.HasPrefix(f.Path, "internal/web/") || f.Generated ||
			strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		lines := strings.Split(f.Body, "\n")
		for i := 0; i < len(lines); i++ {
			line := stripTrailingComment(lines[i])
			if !strings.Contains(line, "if !sameOrigin(") {
				continue
			}
			branches++
			// Тело ветки — от строки if до парной закрывающей скобки.
			depth := strings.Count(line, "{") - strings.Count(line, "}")
			var body []string
			for j := i + 1; j < len(lines) && depth > 0; j++ {
				inner := stripTrailingComment(lines[j])
				depth += strings.Count(inner, "{") - strings.Count(inner, "}")
				if depth > 0 {
					body = append(body, inner)
				}
			}
			block := strings.Join(body, "\n")
			if !strings.Contains(block, "denyCrossOrigin(") {
				t.Errorf("%s:%d: ветка !sameOrigin не зовёт h.denyCrossOrigin(w, r) — "+
					"единственный разрешённый ответ: он даёт лог, метрику и страницу разом", f.Path, i+1)
			}
			for _, banned := range []string{"http.Error(", "renderError("} {
				if strings.Contains(block, banned) {
					t.Errorf("%s:%d: ветка !sameOrigin содержит %s — отказ снова станет "+
						"невидимым (находка №37); отвечать обязан h.denyCrossOrigin(w, r)",
						f.Path, i+1, banned)
				}
			}
		}
	}
	if branches < minSameOriginBranches {
		t.Fatalf("найдено %d веток if !sameOrigin, ожидалось ≥%d — обход ослеп",
			branches, minSameOriginBranches)
	}
}
