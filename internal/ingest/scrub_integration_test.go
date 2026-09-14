package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/scrub"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

type fakeIssueSvc struct{ res issue.UpsertResult }

func (f *fakeIssueSvc) Upsert(_ context.Context, _ int64, _, _, _, _, _ string, _ time.Time) (issue.UpsertResult, error) {
	return f.res, nil
}

func (f *fakeIssueSvc) Get(_ context.Context, id int64) (issue.Issue, error) {
	return issue.Issue{ID: id, TimesSeen: 1}, nil
}

type fakeBatcher struct{ evs []event.Event }

func (f *fakeBatcher) Add(e event.Event) { f.evs = append(f.evs, e) }

func TestPipelineScrubEvent(t *testing.T) {
	fb := &fakeBatcher{}
	p := &Pipeline{
		issues:  &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}},
		batcher: fb,
		Scrub:   scrub.NewScrubber(true, false, []string{"password", "token", "api_key", "authorization", "cookie"}),
	}

	ev := &ParsedEvent{
		EventID:        "e1",
		Timestamp:      time.Now().UTC(),
		UserIP:         "1.2.3.4",
		UserEmail:      "bob@example.com",
		Tags:           map[string]string{"password": "hunter2", "user": "bob"},
		ContextsJSON:   `{"trace":{"token":"secret","ok":1}}`,
		StacktraceJSON: `{"frames":[]}`,
		RequestJSON: `{"method":"POST",` +
			`"url":"https://app/reset?token=SECRET&next=/home",` +
			`"query_string":"api_key=AKIA&q=ok",` +
			`"headers":{"Authorization":"Bearer z","X-Api-Key":"AKIA","Accept":"*/*"},` +
			`"cookies":"sid=abc",` +
			`"data":"username=bob&password=hunter2"}`,
	}

	p.process(task{projectID: 1, ev: ev})

	if len(fb.evs) != 1 {
		t.Fatalf("до батчера дошло событий = %d, want 1", len(fb.evs))
	}
	got := fb.evs[0]

	if got.UserIP != "" {
		t.Errorf("UserIP = %q, want пусто (ScrubIP=true)", got.UserIP)
	}
	if got.UserEmail != "bob@example.com" {
		t.Errorf("UserEmail = %q, want не тронут (ScrubEmail=false)", got.UserEmail)
	}
	if got.Tags["password"] != "[scrubbed]" {
		t.Errorf("tags[password] = %q, want %q", got.Tags["password"], "[scrubbed]")
	}
	if got.Tags["user"] != "bob" {
		t.Errorf("tags[user] = %q, want не тронут", got.Tags["user"])
	}

	var ctx map[string]any
	if err := json.Unmarshal([]byte(got.Contexts), &ctx); err != nil {
		t.Fatalf("contexts не JSON: %v", err)
	}
	tr, _ := ctx["trace"].(map[string]any)
	if tr["token"] != "[scrubbed]" {
		t.Errorf("contexts.trace.token = %v, want %q", tr["token"], "[scrubbed]")
	}
	if tr["ok"] == nil {
		t.Errorf("contexts.trace.ok пропал — не-denylist поле не должно тереться")
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(got.Request), &req); err != nil {
		t.Fatalf("request не JSON: %v", err)
	}
	if got, want := req["url"], "https://app/reset?token=[scrubbed]&next=/home"; got != want {
		t.Errorf("request.url = %v, want %q (token в query вычищен, путь цел)", got, want)
	}
	if got, want := req["query_string"], "api_key=[scrubbed]&q=ok"; got != want {
		t.Errorf("request.query_string = %v, want %q", got, want)
	}
	if got, want := req["data"], "username=bob&password=[scrubbed]"; got != want {
		t.Errorf("request.data = %v, want %q (password в теле формы вычищен, username цел)", got, want)
	}
	if req["cookies"] != "[scrubbed]" {
		t.Errorf("request.cookies = %v, want %q", req["cookies"], "[scrubbed]")
	}
	hdr, _ := req["headers"].(map[string]any)
	if hdr["Authorization"] != "[scrubbed]" {
		t.Errorf("headers.Authorization = %v, want %q", hdr["Authorization"], "[scrubbed]")
	}
	if hdr["X-Api-Key"] != "[scrubbed]" {
		t.Errorf("headers.X-Api-Key = %v, want %q (дефисный ключ ловится нормализацией)", hdr["X-Api-Key"], "[scrubbed]")
	}
	if hdr["Accept"] != "*/*" {
		t.Errorf("headers.Accept = %v, want не тронут", hdr["Accept"])
	}
}

func TestPipelineScrubTransaction(t *testing.T) {
	spans := &fakeSpanSink{}
	p := &Pipeline{
		Spans: spans,
		Scrub: scrub.NewScrubber(true, false, []string{"authorization"}),
	}

	start := time.Now().UTC()
	tx := trace.Transaction{
		TraceID: "t1", SpanID: "root", Name: "GET /x", Op: "http.server",
		Start: start, End: start.Add(time.Second),
		Spans: []trace.Span{{
			SpanID: "a", ParentSpanID: "root", Op: "http.client",
			Data: map[string]any{"http.authorization": "Bearer xyz", "http.status_code": 200},
		}},
	}

	p.processTransaction(1, 1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	got := spans.added[0]
	d := got.Spans[0].Data
	if d["http.authorization"] != "[scrubbed]" {
		t.Errorf("span.Data[http.authorization] = %v, want %q", d["http.authorization"], "[scrubbed]")
	}
	if d["http.status_code"] != 200 {
		t.Errorf("span.Data[http.status_code] = %v, want 200 (не тронут)", d["http.status_code"])
	}
}

func TestPipelineScrubFreeTextEvent(t *testing.T) {
	makePipeline := func(freeText bool) (*Pipeline, *fakeBatcher) {
		fb := &fakeBatcher{}
		sc := scrub.NewScrubber(false, false, nil)
		sc.ScrubFreeText = freeText
		return &Pipeline{
			issues:  &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}},
			batcher: fb,
			Scrub:   sc,
		}, fb
	}
	newEvent := func() *ParsedEvent {
		return &ParsedEvent{
			EventID:   "e1",
			Timestamp: time.Now().UTC(),
			Message:   "error for user@example.com",
			Exceptions: []fingerprint.Exception{
				{Type: "ValueError", Value: "bad addr admin@corp.io in payload"},
			},
		}
	}

	p, fb := makePipeline(true)
	p.process(task{projectID: 1, ev: newEvent()})
	if len(fb.evs) != 1 {
		t.Fatalf("до батчера дошло событий = %d, want 1", len(fb.evs))
	}
	if got := fb.evs[0].Message; got != "error for [email]" {
		t.Errorf("Message = %q, want %q", got, "error for [email]")
	}
	if got := fb.evs[0].ExceptionValue; got != "bad addr [email] in payload" {
		t.Errorf("ExceptionValue = %q, want %q", got, "bad addr [email] in payload")
	}

	p, fb = makePipeline(false)
	p.process(task{projectID: 1, ev: newEvent()})
	if got := fb.evs[0].Message; got != "error for user@example.com" {
		t.Errorf("при выключенном флаге Message = %q, want не тронут", got)
	}
	if got := fb.evs[0].ExceptionValue; got != "bad addr admin@corp.io in payload" {
		t.Errorf("при выключенном флаге ExceptionValue = %q, want не тронут", got)
	}
}

func TestPipelineScrubTransactionName(t *testing.T) {
	makePipeline := func(freeText bool) (*Pipeline, *fakeSpanSink) {
		spans := &fakeSpanSink{}
		sc := scrub.NewScrubber(false, false, nil)
		sc.ScrubFreeText = freeText
		return &Pipeline{Spans: spans, Scrub: sc}, spans
	}
	newTx := func() trace.Transaction {
		start := time.Now().UTC()
		return trace.Transaction{
			TraceID: "t1", SpanID: "root",
			Name:  "GET /u?token=secret&email=a@b.com",
			Op:    "http.server",
			Start: start, End: start.Add(time.Second),
		}
	}

	p, spans := makePipeline(true)
	p.processTransaction(1, 1, newTx())
	if got := spans.added[0].Name; got != "GET /u?token=secret&email=[email]" {
		t.Errorf("tx.Name = %q, want email замаскированным на [email]", got)
	}

	p, spans = makePipeline(false)
	p.processTransaction(1, 1, newTx())
	if got := spans.added[0].Name; got != "GET /u?token=secret&email=a@b.com" {
		t.Errorf("при выключенном флаге tx.Name = %q, want не тронут", got)
	}
}

type capturingIssueSvc struct {
	res         issue.UpsertResult
	title       string
	culprit     string
	upsertCalls int
}

func (f *capturingIssueSvc) Upsert(_ context.Context, _ int64, _, title, culprit, _, _ string, _ time.Time) (issue.UpsertResult, error) {
	f.upsertCalls++
	f.title = title
	f.culprit = culprit
	return f.res, nil
}

func (f *capturingIssueSvc) Get(_ context.Context, id int64) (issue.Issue, error) {
	return issue.Issue{ID: id, TimesSeen: 1}, nil
}

type capturingAlertSink struct {
	called bool
	ev     alert.Event
}

func (f *capturingAlertSink) OnIssue(_ context.Context, ev alert.Event) {
	f.called = true
	f.ev = ev
}

func TestPipelineScrubFreeTextTitleBeforeUpsert(t *testing.T) {
	newEvent := func() *ParsedEvent {
		return &ParsedEvent{
			EventID:   "e1",
			Timestamp: time.Now().UTC(),
			Title:     "ValueError: bad addr admin@corp.io in payload",
			Culprit:   "app.handlers.save",
			Exceptions: []fingerprint.Exception{
				{Type: "ValueError", Value: "bad addr admin@corp.io in payload"},
			},
		}
	}

	t.Run("on", func(t *testing.T) {
		sc := scrub.NewScrubber(false, false, nil)
		sc.ScrubFreeText = true
		iss := &capturingIssueSvc{res: issue.UpsertResult{IssueID: 1, New: true}}
		alerts := &capturingAlertSink{}
		p := &Pipeline{issues: iss, batcher: &fakeBatcher{}, Alerts: alerts, Scrub: sc}

		p.process(task{projectID: 1, ev: newEvent()})

		want := "ValueError: bad addr [email] in payload"
		if iss.title != want {
			t.Errorf("Upsert title = %q, want %q", iss.title, want)
		}
		if !alerts.called {
			t.Fatal("OnIssue не вызван (ожидали New=true)")
		}
		if alerts.ev.Title != want {
			t.Errorf("OnIssue Title = %q, want %q", alerts.ev.Title, want)
		}
	})

	t.Run("off", func(t *testing.T) {
		sc := scrub.NewScrubber(false, false, nil)
		sc.ScrubFreeText = false
		iss := &capturingIssueSvc{res: issue.UpsertResult{IssueID: 1, New: true}}
		alerts := &capturingAlertSink{}
		p := &Pipeline{issues: iss, batcher: &fakeBatcher{}, Alerts: alerts, Scrub: sc}

		p.process(task{projectID: 1, ev: newEvent()})

		want := "ValueError: bad addr admin@corp.io in payload"
		if iss.title != want {
			t.Errorf("при выключенном флаге Upsert title = %q, want не тронут", iss.title)
		}
		if alerts.ev.Title != want {
			t.Errorf("при выключенном флаге OnIssue Title = %q, want не тронут", alerts.ev.Title)
		}
	})
}

func TestScrubFreeTextDoesNotChangeFingerprint(t *testing.T) {
	in := fingerprint.Input{
		Exceptions: []fingerprint.Exception{
			{Type: "ValueError", Value: "bad addr admin@corp.io in payload"},
		},
	}
	if got, want := fingerprint.Compute(in), fingerprint.Compute(in); got != want {
		t.Fatalf("fingerprint нестабилен: %q != %q", got, want)
	}
}

func TestPipelineScrubFreeTextTransaction(t *testing.T) {
	spans := &fakeSpanSink{}
	sc := scrub.NewScrubber(false, false, nil)
	sc.ScrubFreeText = true
	p := &Pipeline{Spans: spans, Scrub: sc}

	start := time.Now().UTC()
	tx := trace.Transaction{
		TraceID: "t1", SpanID: "root", Name: "GET /x", Op: "http.server",
		Start: start, End: start.Add(time.Second),
		Spans: []trace.Span{{
			SpanID: "a", ParentSpanID: "root", Op: "http.client",
			Description: "GET /users?email=user@example.com",
		}},
	}

	p.processTransaction(1, 1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	if got := spans.added[0].Spans[0].Description; got != "GET /users?email=[email]" {
		t.Errorf("span.Description = %q, want %q", got, "GET /users?email=[email]")
	}
}

func TestPipelineScrubTransactionTags(t *testing.T) {
	spans := &fakeSpanSink{}
	p := &Pipeline{
		Spans: spans,
		Scrub: scrub.NewScrubber(true, false, []string{"authorization"}),
	}

	start := time.Now().UTC()
	tx := trace.Transaction{
		TraceID: "t1", SpanID: "root", Name: "GET /x", Op: "http.server",
		Start: start, End: start.Add(time.Second),
		Tags: map[string]string{"authorization": "Bearer x", "service": "api"},
	}

	p.processTransaction(1, 1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	got := spans.added[0]
	if got.Tags["authorization"] != "[scrubbed]" {
		t.Errorf("tags[authorization] = %q, want %q", got.Tags["authorization"], "[scrubbed]")
	}
	if got.Tags["service"] != "api" {
		t.Errorf("tags[service] = %q, want не тронут", got.Tags["service"])
	}
}
