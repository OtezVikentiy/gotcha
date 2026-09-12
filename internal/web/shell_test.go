package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestWithShellSkipsStaticAndAnonymous(t *testing.T) {
	h := &Handler{} // Auth/Org nil: запросы БЕЗ сессионной cookie не трогают их

	var seen nav.Shell
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nav.FromContext(r.Context())
		w.WriteHeader(200)
	})
	mw := h.withShell(next)

	seen = nav.Shell{}
	rs := httptest.NewRequest("GET", "/static/app.css", nil)
	mw.ServeHTTP(httptest.NewRecorder(), rs)
	if seen.Area != "" {
		t.Fatalf("static should skip resolve, got area = %q", seen.Area)
	}

	seen = nav.Shell{}
	r := httptest.NewRequest("GET", "/projects/1/issues", nil)
	mw.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Area != "" {
		t.Fatalf("anonymous request should skip resolve, got area = %q", seen.Area)
	}
}

func projCookieHeader(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "proj" {
			return c.Value
		}
	}
	return ""
}

func TestWithShellStickyProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := &Handler{Auth: authSvc, Org: orgSvc, BaseURL: "http://localhost"}
	ctx := context.Background()

	uid, err := authSvc.Register(ctx, "sticky@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	o, err := orgSvc.CreateOrg(ctx, "sticky", "Sticky Org", uid)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := orgSvc.CreateProject(ctx, o.ID, "first", "First", "go")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := orgSvc.CreateProject(ctx, o.ID, "second", "Second", "go")
	if err != nil {
		t.Fatal(err)
	}
	token, err := authSvc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	var seen nav.Shell
	mw := h.withShell(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nav.FromContext(r.Context())
	}))
	get := func(path string, projCookie string) *httptest.ResponseRecorder {
		t.Helper()
		seen = nav.Shell{}
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		if projCookie != "" {
			r.AddCookie(&http.Cookie{Name: "proj", Value: projCookie})
		}
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		return w
	}

	p2str := strconv.FormatInt(p2.ID, 10)

	w := get("/projects/"+p2str+"/issues", "")
	if seen.ProjectID != p2.ID {
		t.Fatalf("ProjectID = %d, want %d (from path)", seen.ProjectID, p2.ID)
	}
	if got := projCookieHeader(w); got != p2str {
		t.Fatalf("proj cookie after project page = %q, want %q", got, p2str)
	}

	w = get("/projects/"+p2str+"/issues", p2str)
	if got := projCookieHeader(w); got != "" {
		t.Fatalf("proj cookie rewritten to %q, want no Set-Cookie", got)
	}

	w = get("/issues/9", p2str)
	if seen.ProjectID != p2.ID {
		t.Fatalf("ProjectID on detail page = %d, want %d (sticky)", seen.ProjectID, p2.ID)
	}
	if got := projCookieHeader(w); got != "" {
		t.Fatalf("detail page must not touch proj cookie, got %q", got)
	}

	get("/issues/9", "garbage")
	if seen.ProjectID != 0 {
		t.Fatalf("ProjectID with garbage cookie = %d, want 0", seen.ProjectID)
	}

	foreign := strconv.FormatInt(p2.ID+12345, 10)
	get("/issues/9", foreign)
	if seen.ProjectID != 0 {
		t.Fatalf("ProjectID with foreign cookie = %d, want 0", seen.ProjectID)
	}

	// Чужой/несуществующий проект в пути не должен портить cookie: хендлер ответит 404, но
	// запомненный выбор обязан пережить такой заход.
	w = get("/projects/"+foreign+"/issues", p2str)
	if got := projCookieHeader(w); got != "" {
		t.Fatalf("foreign project in path wrote proj cookie %q, want none", got)
	}

	_ = p1 // первый проект нужен только как «дефолт», к которому не должно откатывать
}

// sh.Orgs несёт все организации, sh.Projects сужается ВЫБРАННОЙ (OrgID) — иначе селект
// организации и переключатель проекта под ним противоречили бы друг другу.
func TestWithShellNarrowsProjectsByOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := &Handler{Auth: authSvc, Org: orgSvc, BaseURL: "http://localhost"}
	ctx := context.Background()

	uid, err := authSvc.Register(ctx, "orgscope-web@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	orgA, err := orgSvc.CreateOrg(ctx, "orgscope-web-a", "Org A", uid)
	if err != nil {
		t.Fatal(err)
	}
	orgB, err := orgSvc.CreateOrg(ctx, "orgscope-web-b", "Org B", uid)
	if err != nil {
		t.Fatal(err)
	}
	projA, err := orgSvc.CreateProject(ctx, orgA.ID, "proj-a", "Proj A", "go")
	if err != nil {
		t.Fatal(err)
	}
	projB, err := orgSvc.CreateProject(ctx, orgB.ID, "proj-b", "Proj B", "go")
	if err != nil {
		t.Fatal(err)
	}
	token, err := authSvc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	var seen nav.Shell
	mw := h.withShell(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nav.FromContext(r.Context())
	}))
	get := func(path string) {
		t.Helper()
		seen = nav.Shell{}
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		mw.ServeHTTP(httptest.NewRecorder(), r)
	}

	get("/projects/" + strconv.FormatInt(projA.ID, 10) + "/issues")
	if len(seen.Orgs) != 2 {
		t.Fatalf("Orgs = %+v, want 2", seen.Orgs)
	}

	if len(seen.Projects) != 1 || seen.Projects[0].ID != projA.ID {
		t.Fatalf("Projects with orgA selected = %+v, want only projA", seen.Projects)
	}

	get("/projects/" + strconv.FormatInt(projB.ID, 10) + "/issues")
	if len(seen.Projects) != 1 || seen.Projects[0].ID != projB.ID {
		t.Fatalf("Projects with orgB selected = %+v, want only projB", seen.Projects)
	}
}

// projID из cookie может относиться к другой организации, чем видна из пути — без сброса
// шапка и рейл расходятся молча, и рейл ведёт в чужую организацию.
func TestWithShellDropsForeignOrgProjectCookie(t *testing.T) {
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := &Handler{Auth: authSvc, Org: orgSvc, BaseURL: "http://localhost"}
	ctx := context.Background()

	uid, err := authSvc.Register(ctx, "foreign-org-cookie@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	orgA, err := orgSvc.CreateOrg(ctx, "foreign-cookie-a", "Probe A", uid)
	if err != nil {
		t.Fatal(err)
	}
	orgB, err := orgSvc.CreateOrg(ctx, "foreign-cookie-b", "Probe B", uid)
	if err != nil {
		t.Fatal(err)
	}
	projA, err := orgSvc.CreateProject(ctx, orgA.ID, "proj-a", "Proj A", "go")
	if err != nil {
		t.Fatal(err)
	}
	projB, err := orgSvc.CreateProject(ctx, orgB.ID, "proj-b", "Proj B", "go")
	if err != nil {
		t.Fatal(err)
	}
	token, err := authSvc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	var seen nav.Shell
	mw := h.withShell(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nav.FromContext(r.Context())
	}))

	r := httptest.NewRequest("GET", "/orgs/"+strconv.FormatInt(orgA.ID, 10)+"/projects", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	r.AddCookie(&http.Cookie{Name: "proj", Value: strconv.FormatInt(projB.ID, 10)})
	mw.ServeHTTP(httptest.NewRecorder(), r)

	if seen.OrgID != orgA.ID {
		t.Fatalf("OrgID = %d, want %d (org A, from path)", seen.OrgID, orgA.ID)
	}
	if seen.ProjectID == projB.ID {
		t.Fatalf("ProjectID = %d, want reset away from foreign-org project B", seen.ProjectID)
	}
	if len(seen.Projects) != 1 || seen.Projects[0].ID != projA.ID {
		t.Fatalf("Projects = %+v, want only projA (org A)", seen.Projects)
	}
	projAPart := "/projects/" + strconv.FormatInt(projA.ID, 10) + "/"
	projBPart := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/"
	// Рейл должен откатиться на первый проект ТЕКУЩЕЙ организации, совпав с топбаром — не
	// остаться на проекте чужой организации.
	sawProjA := false
	for _, area := range nav.Areas(seen) {
		if strings.Contains(area.Href, projBPart) {
			t.Fatalf("rail area %q hrefs into foreign-org project B: %q", area.ID, area.Href)
		}
		if strings.Contains(area.Href, projAPart) {
			sawProjA = true
		}
	}
	if !sawProjA {
		t.Fatalf("no rail area links into projA, want at least overview")
	}
}

func TestProjectIDFromPath(t *testing.T) {
	cases := map[string]int64{
		"/projects/7/issues": 7,
		"/projects/7":        7,
		"/issues/9":          0,
		"/orgs/5/teams":      0,
		"/profile":           0,
		"/":                  0,
	}
	for path, want := range cases {
		if got := projectIDFromPath(path); got != want {
			t.Errorf("projectIDFromPath(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestOrgIDFromPath(t *testing.T) {
	cases := map[string]int64{
		"/orgs/5/teams":      5,
		"/orgs/5":            5,
		"/projects/7/issues": 0,
		"/profile":           0,
		"/":                  0,
	}
	for path, want := range cases {
		if got := orgIDFromPath(path); got != want {
			t.Errorf("orgIDFromPath(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestBackOrigin(t *testing.T) {
	const base = "https://gotcha.example"
	cur := "/monitors/9"

	cases := []struct {
		name    string
		referer string
		want    string
	}{
		{"empty referer falls back to parent", "", ""},
		{"cross-origin ignored", "https://evil.example/monitors", ""},
		{"same path is a reload, not a back-target", base + "/monitors/9", ""},
		{"static asset ignored", base + "/static/app.css", ""},
		{"login page ignored", base + "/login", ""},
		{"same-origin page kept", base + "/projects/7/incidents", "/projects/7/incidents"},
		{"query string preserved", base + "/projects/7/issues?status=resolved&page=2", "/projects/7/issues?status=resolved&page=2"},
		// Протокол-относительные формы: браузер прочтёт их как чужой адрес — тот же инвариант,
		// что у safeNextPath и BulkRedirectTarget.
		{"protocol-relative rejected", base + "//evil.example/x", ""},
		{"backslash form rejected", base + "/\\evil.example/x", ""},
		{"encoded backslash form rejected", base + "/%5Cevil.example/x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", cur, nil)
			if c.referer != "" {
				r.Header.Set("Referer", c.referer)
			}
			if got := backOrigin(r, base, cur); got != c.want {
				t.Errorf("backOrigin(referer=%q) = %q, want %q", c.referer, got, c.want)
			}
		})
	}
}

// Имя транзакции в URL %-кодировано, curPath декодирован. Сравнение идёт по декодированному
// пути (иначе крошка ссылалась бы сама на себя), ссылка строится по escaped-форме.
func TestBackOriginEncodedPath(t *testing.T) {
	const base = "https://gotcha.example"
	cur := "/projects/7/performance/GET /api/users" // decoded r.URL.Path
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Referer", base+"/projects/7/performance/GET%20%2Fapi%2Fusers")
	if got := backOrigin(r, base, cur); got != "" {
		t.Errorf("encoded self-reference must be treated as reload, got %q", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Referer", base+"/projects/7/performance/POST%20%2Fpay")
	if got, want := backOrigin(r2, base, cur), "/projects/7/performance/POST%20%2Fpay"; got != want {
		t.Errorf("different encoded page: got %q, want %q (escaped preserved)", got, want)
	}
}
