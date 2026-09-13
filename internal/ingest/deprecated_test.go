package ingest

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	pp "github.com/google/pprof/profile"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
)

func serveIngest(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func ndjsonRequest(path string) *http.Request {
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"message":"hi"}`+"\n"))
	req.Header.Set("Authorization", "Bearer pub")
	return req
}

// profile.Write отдаёт уже gzip'нутый protobuf — то, что ждёт pprofIngest.
func validPprofBody(t *testing.T) []byte {
	t.Helper()
	fn := &pp.Function{ID: 1, Name: "main", Filename: "m.go"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
		Sample:     []*pp.Sample{{Location: []*pp.Location{loc}, Value: []int64{5}}},
	}
	var buf bytes.Buffer
	if err := prof.Write(&buf); err != nil {
		t.Fatalf("write pprof: %v", err)
	}
	return buf.Bytes()
}

func assertDeprecated(t *testing.T, rec *httptest.ResponseRecorder, wantDocs string) {
	t.Helper()
	if got := rec.Header().Get("Deprecation"); got != "@1788134400" {
		t.Errorf("Deprecation = %q, want %q", got, "@1788134400")
	}
	link := rec.Header().Get("Link")
	if !strings.Contains(link, "<"+wantDocs+">") || !strings.Contains(link, `rel="deprecation"`) {
		t.Errorf("Link = %q, want <%s>; rel=\"deprecation\"", link, wantDocs)
	}
}

func assertNotDeprecated(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Deprecation"); got != "" {
		t.Errorf("канон поставил Deprecation = %q, хотя не должен", got)
	}
	if got := rec.Header().Get("Link"); got != "" {
		t.Errorf("канон поставил Link = %q, хотя не должен", got)
	}
}

func TestDeprecatedAliasLogs(t *testing.T) {
	h := newRejectHandler(1 << 20)
	h.Logs = &collectLogSink{}

	before := h.DeprecatedPathHits(DeprecatedLogs)

	canon := serveIngest(h, ndjsonRequest("/api/v1/logs"))
	if canon.Code != http.StatusOK {
		t.Fatalf("канон: status = %d, body = %s", canon.Code, canon.Body.String())
	}
	assertNotDeprecated(t, canon)
	if got := h.DeprecatedPathHits(DeprecatedLogs); got != before {
		t.Errorf("канон сдвинул счётчик: %d, want %d", got, before)
	}

	alias := serveIngest(h, ndjsonRequest("/logs"))
	if alias.Code != canon.Code {
		t.Fatalf("алиас: status = %d, want %d (как канон)", alias.Code, canon.Code)
	}
	if alias.Body.String() != canon.Body.String() {
		t.Errorf("алиас: body = %q, want %q (как канон)", alias.Body.String(), canon.Body.String())
	}
	assertDeprecated(t, alias, "/docs/logs")
	if got := h.DeprecatedPathHits(DeprecatedLogs); got != before+1 {
		t.Errorf("DeprecatedPathHits(/logs) = %d, want %d", got, before+1)
	}
}

func TestDeprecatedAliasPprof(t *testing.T) {
	h := newRejectHandler(1 << 20)
	sink := &countingProfileSink{}
	h.Profiles = sink
	body := validPprofBody(t)

	pprofReq := func(path string) *http.Request {
		req := httptest.NewRequest("POST", path+"?service=api", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer pub")
		req.Header.Set("Content-Type", "application/octet-stream")
		return req
	}

	before := h.DeprecatedPathHits(DeprecatedProfilePprof)

	canon := serveIngest(h, pprofReq("/api/v1/profiles/pprof"))
	if canon.Code != http.StatusAccepted {
		t.Fatalf("канон: status = %d, want 202, body = %s", canon.Code, canon.Body.String())
	}
	assertNotDeprecated(t, canon)
	if got := h.DeprecatedPathHits(DeprecatedProfilePprof); got != before {
		t.Errorf("канон сдвинул счётчик: %d, want %d", got, before)
	}
	acceptedByCanon := sink.n

	alias := serveIngest(h, pprofReq("/profiles/pprof"))
	if alias.Code != canon.Code {
		t.Fatalf("алиас: status = %d, want %d (как канон)", alias.Code, canon.Code)
	}
	if sink.n != acceptedByCanon+1 {
		t.Errorf("алиас не дошёл до приёмника профилей: принято %d, want %d", sink.n, acceptedByCanon+1)
	}
	assertDeprecated(t, alias, "/docs/profiling")
	if got := h.DeprecatedPathHits(DeprecatedProfilePprof); got != before+1 {
		t.Errorf("DeprecatedPathHits(/profiles/pprof) = %d, want %d", got, before+1)
	}
}

func TestDeprecatedAliasDeployments(t *testing.T) {
	h, projectID := newIngestTestWithDeploy(t)
	id := strconv.FormatInt(projectID, 10)
	body := func() io.Reader {
		return strings.NewReader(`{"version":"v9.9.9","environment":"prod"}`)
	}
	deployReq := func(path string) *http.Request {
		return httptest.NewRequest("POST", path+"?sentry_key=deadbeef", body())
	}

	before := h.DeprecatedPathHits(DeprecatedDeployments)

	canon := serveIngest(h, deployReq("/api/v1/"+id+"/deployments"))
	if canon.Code != http.StatusOK {
		t.Fatalf("канон без слэша: status = %d, body = %s", canon.Code, canon.Body.String())
	}
	assertNotDeprecated(t, canon)

	slashed := serveIngest(h, deployReq("/api/v1/"+id+"/deployments/"))
	if slashed.Code != http.StatusOK {
		t.Fatalf("канон со слэшем: status = %d, body = %s", slashed.Code, slashed.Body.String())
	}
	assertNotDeprecated(t, slashed)

	if got := h.DeprecatedPathHits(DeprecatedDeployments); got != before {
		t.Errorf("канон сдвинул счётчик: %d, want %d", got, before)
	}

	alias := serveIngest(h, deployReq("/api/"+id+"/deployments/"))
	if alias.Code != http.StatusOK {
		t.Fatalf("алиас: status = %d, body = %s", alias.Code, alias.Body.String())
	}
	assertDeprecated(t, alias, "/docs/deployments")
	if got := h.DeprecatedPathHits(DeprecatedDeployments); got != before+1 {
		t.Errorf("DeprecatedPathHits(deployments) = %d, want %d", got, before+1)
	}
}

func TestDeprecatedAliasLogsOnce(t *testing.T) {
	h := newRejectHandler(1 << 20)
	h.Logs = &collectLogSink{}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	serveIngest(h, ndjsonRequest("/logs"))
	first := buf.String()
	if !strings.Contains(first, "path=/logs") || !strings.Contains(first, "/api/v1/logs") {
		t.Fatalf("первый запрос не дал предупреждения со старым и новым путём:\n%s", first)
	}

	serveIngest(h, ndjsonRequest("/logs"))
	if second := buf.String(); second != first {
		t.Errorf("второй запрос на тот же алиас дописал в лог:\nбыло:\n%s\nстало:\n%s", first, second)
	}
	if got := h.DeprecatedPathHits(DeprecatedLogs); got != 2 {
		t.Errorf("счётчик = %d, want 2: лог один раз, метрика — на каждый запрос", got)
	}
}

func TestDeprecatedPathsIsCopy(t *testing.T) {
	got := DeprecatedPaths()
	if len(got) != 3 {
		t.Fatalf("DeprecatedPaths() вернул %d путей, want 3: %v", len(got), got)
	}
	got[0] = "мусор"
	if again := DeprecatedPaths(); again[0] == "мусор" {
		t.Error("DeprecatedPaths вернул общий слайс, а не копию")
	}
}

func TestDeprecatedPathHitsUnknownPath(t *testing.T) {
	h := newRejectHandler(1 << 20)
	if got := h.DeprecatedPathHits(DeprecatedPath("/nowhere")); got != 0 {
		t.Errorf("DeprecatedPathHits(/nowhere) = %d, want 0 (пути нет в наборе)", got)
	}
}

func TestDocsPath(t *testing.T) {
	cases := []struct {
		path     DeprecatedPath
		wantDocs string
		wantOK   bool
	}{
		{DeprecatedLogs, "/docs/logs", true},
		{DeprecatedProfilePprof, "/docs/profiling", true},
		{DeprecatedDeployments, "/docs/deployments", true},
		{DeprecatedPath("/nowhere"), "", false},
	}
	for _, c := range cases {
		docs, ok := DocsPath(c.path)
		if docs != c.wantDocs || ok != c.wantOK {
			t.Errorf("DocsPath(%q) = (%q, %v), want (%q, %v)", c.path, docs, ok, c.wantDocs, c.wantOK)
		}
	}
}

func TestPathForDeprecatedKind(t *testing.T) {
	cases := []struct {
		kind     ingestsignal.Kind
		wantPath DeprecatedPath
		wantOK   bool
	}{
		{ingestsignal.KindDeprecatedLogs, DeprecatedLogs, true},
		{ingestsignal.KindDeprecatedPprof, DeprecatedProfilePprof, true},
		{ingestsignal.KindDeprecatedDeployments, DeprecatedDeployments, true},
		{ingestsignal.KindKeyInvalid, "", false},
	}
	for _, c := range cases {
		path, ok := PathForDeprecatedKind(c.kind)
		if path != c.wantPath || ok != c.wantOK {
			t.Errorf("PathForDeprecatedKind(%q) = (%q, %v), want (%q, %v)", c.kind, path, ok, c.wantPath, c.wantOK)
		}
	}
}

func TestDeprecatedTargetsAndKindsHaveSameKeys(t *testing.T) {
	for p := range deprecatedTargets {
		if _, ok := deprecatedKinds[p]; !ok {
			t.Errorf("deprecatedTargets содержит %q, а deprecatedKinds — нет", p)
		}
	}
	for p := range deprecatedKinds {
		if _, ok := deprecatedTargets[p]; !ok {
			t.Errorf("deprecatedKinds содержит %q, а deprecatedTargets — нет", p)
		}
	}
}
