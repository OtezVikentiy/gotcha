package guards

import (
	"strings"
	"testing"
)

// ниже порога — обход ослеп (сузился до части файлов), а не ветки вычищены.
const minSameOriginBranches = 55

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
