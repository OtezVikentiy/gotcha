package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestIndexStickyProject — корень "/" уводит на «Обзор» запомненного в
// cookie "proj" проекта (задача 6 nav-ia); недоступный, битый или отсутствующий
// id в cookie откатывает НЕ на первый проект списка (кука голая "/" —
// единственное место, где ей вообще дан голос, §5 спеки — молчаливый откат на
// первый проект подменял бы явный выбор организации), а на список проектов
// первой по порядку организации.
func TestIndexStickyProject(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	orgSvc := org.NewService(s.pool, 1_000_000)
	uid, err := s.h.Auth.Register(ctx, "index-sticky@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	o, err := orgSvc.CreateOrg(ctx, "idx-sticky", "Idx Sticky", uid)
	if err != nil {
		t.Fatal(err)
	}
	// p1 существует только чтобы у организации было больше одного проекта
	// (см. "Мусор"/"Недоступный id" ниже: откат больше не выбирает первый
	// проект списка, поэтому сам p1 нигде дальше не нужен по значению).
	if _, err := orgSvc.CreateProject(ctx, o.ID, "one", "One", "go"); err != nil {
		t.Fatal(err)
	}
	p2, err := orgSvc.CreateProject(ctx, o.ID, "two", "Two", "go")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.h.Auth.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	session := &http.Cookie{Name: auth.CookieName, Value: token}

	getRoot := func(proj string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/", nil)
		req.AddCookie(session)
		if proj != "" {
			req.AddCookie(&http.Cookie{Name: "proj", Value: proj})
		}
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("get /: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET / status = %d, want 303", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}

	overviewPath := func(id int64) string {
		return "/projects/" + strconv.FormatInt(id, 10) + "/overview"
	}
	orgProjects := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/projects"

	// Без cookie — дверь в список проектов организации, не молчаливый выбор
	// первого проекта.
	if got := getRoot(""); got != orgProjects {
		t.Fatalf("GET / (no cookie) Location = %q, want %q", got, orgProjects)
	}
	// С cookie — «Обзор» запомненного проекта.
	if got := getRoot(strconv.FormatInt(p2.ID, 10)); got != overviewPath(p2.ID) {
		t.Fatalf("GET / (proj=p2) Location = %q, want %q", got, overviewPath(p2.ID))
	}
	// Недоступный id — список проектов организации, не первый проект.
	if got := getRoot(strconv.FormatInt(p2.ID+12345, 10)); got != orgProjects {
		t.Fatalf("GET / (foreign proj) Location = %q, want %q", got, orgProjects)
	}
	// Мусор — список проектов организации, не первый проект.
	if got := getRoot("garbage"); got != orgProjects {
		t.Fatalf("GET / (garbage proj) Location = %q, want %q", got, orgProjects)
	}
}
