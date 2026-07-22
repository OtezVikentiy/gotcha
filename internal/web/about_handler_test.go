package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

func TestAboutPageRendersVersion(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/about", nil)
	h.aboutPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ждали 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), version.Version()) {
		t.Fatalf("страница About должна содержать версию %q", version.Version())
	}
}
