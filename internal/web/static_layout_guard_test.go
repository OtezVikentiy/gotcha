package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

var staticRefRe = regexp.MustCompile(`assetURL\("(/static/[^"]+)"\)`)

// Список источник — layout.templ, не хардкод: переименование файла ловится и здесь,
// и добавление нового <script> проверяется без правки теста.
func staticRefsFromLayout(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("templates/layout.templ")
	if err != nil {
		t.Fatalf("read layout.templ: %v", err)
	}
	matches := staticRefRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("в layout.templ не найдено ни одной assetURL(\"/static/...\") — регэксп теста разошёлся с шаблоном")
	}
	refs := make([]string, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, m[1])
	}
	return refs
}

func TestLayoutStaticReferencesExistInEmbedFS(t *testing.T) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	for _, ref := range staticRefsFromLayout(t) {
		name := strings.TrimPrefix(ref, "/static/")
		if _, err := fs.Stat(sub, name); err != nil {
			t.Errorf("layout.templ ссылается на %q, в embed static её нет: %v", ref, err)
		}
	}
}

func TestStaticJSServesFromRealEmbedFS(t *testing.T) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	found := 0
	for _, ref := range staticRefsFromLayout(t) {
		if !strings.HasSuffix(ref, ".js") {
			continue
		}
		found++
		name := strings.TrimPrefix(ref, "/static/")
		req := httptest.NewRequest(http.MethodGet, "/"+name, nil)
		rr := httptest.NewRecorder()
		fileServer.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("GET /static/%s (реальный embed.FS) = %d, want 200", name, rr.Code)
		}
		if rr.Body.Len() == 0 {
			t.Errorf("GET /static/%s вернул пустое тело", name)
		}
	}
	if found == 0 {
		t.Fatal("в layout.templ не найдено ни одного <script src> .js — тест ничего не проверил")
	}
}
