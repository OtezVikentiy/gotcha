package web_test

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

var depsTableRowRe = regexp.MustCompile(`(?s)<table[^>]*data-table.*postgresql`)

// SELECT+INSERT к одному engine → направление «читаем и пишем» (arrow-left-right).
var depsDirBothRe = regexp.MustCompile(`postgresql</td>\s*<td><span class="deps-dir"[^>]*title="читаем и пишем"[^>]*aria-label="читаем и пишем"[^>]*><svg class="ic"[^>]*><use href="#i-arrow-left-right"></use></svg></span></td>`)

// Без метода/описания направление не определяется — иконка minus, не пустая ячейка.
var depsDirNoneRe = regexp.MustCompile(`api\.stripe\.com</td>\s*<td><span class="deps-dir"[^>]*title="не определено"[^>]*><svg class="ic"[^>]*><use href="#i-minus"></use></svg></span></td>`)

func TestWebDependenciesScreen(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "deps-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "deps-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "deps-co", "Deps Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "deps-proj", "Deps Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	at := time.Now().UTC().Add(-30 * time.Minute)
	s.writer.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID: "deps-trace", SpanID: "deps-root", Name: "GET /api/checkout", Op: "http.server",
		Status: "ok", Start: at, End: at.Add(200 * time.Millisecond), Environment: "production",
		Spans: []trace.Span{
			{SpanID: "deps-db1", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT * FROM carts WHERE id = $1",
				Start:       at, End: at.Add(3000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "postgresql"}},
			{SpanID: "deps-db2", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "INSERT INTO orders (cart_id) VALUES ($1)",
				Start:       at, End: at.Add(2000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "postgresql"}},
			{SpanID: "deps-stripe", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Start: at, End: at.Add(60000 * time.Microsecond),
				Data: map[string]any{"server.address": "api.stripe.com"}},
		},
	})
	s.flush(t)

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/dependencies"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"postgresql", "api.stripe.com"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}
	if !depsTableRowRe.MatchString(string(body)) {
		t.Fatalf("GET %s (owner) table missing dependency row: %s", path, body)
	}
	for _, want := range []string{`<svg class="deps-map`, `deps-legend`, `<th scope="col">Данные</th>`,
		`<use href="#i-arrow-left"></use></svg>читаем из зависимости`,
		`<use href="#i-arrow-right"></use></svg>пишем в зависимость`,
		`<use href="#i-arrow-left-right"></use></svg>и то и другое`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}
	if !depsDirBothRe.MatchString(string(body)) {
		t.Fatalf("GET %s (owner) postgresql row lacks arrow-left-right direction icon: %s", path, body)
	}
	if !depsDirNoneRe.MatchString(string(body)) {
		t.Fatalf("GET %s (owner) api.stripe.com row lacks minus (none) icon: %s", path, body)
	}

	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

func TestWebDependenciesEmpty(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "deps-empty-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "deps-empty-co", "Deps Empty Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "deps-empty-proj", "Deps Empty Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/dependencies"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (empty) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Зависимостей нет") {
		t.Fatalf("GET %s (empty) missing empty state: %s", path, body)
	}
}
