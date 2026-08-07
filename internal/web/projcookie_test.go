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

// TestIndexStickyProject — корень "/" уводит на запомненный в cookie "proj"
// проект, а не всегда на первый из списка; недоступный или битый id в cookie
// молча откатывает на первый.
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
	p1, err := orgSvc.CreateProject(ctx, o.ID, "one", "One", "go")
	if err != nil {
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

	issuesPath := func(id int64) string {
		return "/projects/" + strconv.FormatInt(id, 10) + "/issues"
	}

	// Без cookie — прежнее поведение: первый проект.
	if got := getRoot(""); got != issuesPath(p1.ID) {
		t.Fatalf("GET / (no cookie) Location = %q, want %q", got, issuesPath(p1.ID))
	}
	// С cookie — запомненный проект.
	if got := getRoot(strconv.FormatInt(p2.ID, 10)); got != issuesPath(p2.ID) {
		t.Fatalf("GET / (proj=p2) Location = %q, want %q", got, issuesPath(p2.ID))
	}
	// Недоступный id — откат на первый.
	if got := getRoot(strconv.FormatInt(p2.ID+12345, 10)); got != issuesPath(p1.ID) {
		t.Fatalf("GET / (foreign proj) Location = %q, want %q", got, issuesPath(p1.ID))
	}
	// Мусор — откат на первый.
	if got := getRoot("garbage"); got != issuesPath(p1.ID) {
		t.Fatalf("GET / (garbage proj) Location = %q, want %q", got, issuesPath(p1.ID))
	}
}
