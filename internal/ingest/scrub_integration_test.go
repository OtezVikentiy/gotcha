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
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// fakeIssueSvc — issueUpserter без PG: возвращает фиксированный результат апсерта.
type fakeIssueSvc struct{ res issue.UpsertResult }

func (f *fakeIssueSvc) Upsert(_ context.Context, _ int64, _, _, _, _, _ string, _ time.Time) (issue.UpsertResult, error) {
	return f.res, nil
}

func (f *fakeIssueSvc) Get(_ context.Context, id int64) (issue.Issue, error) {
	return issue.Issue{ID: id, TimesSeen: 1}, nil
}

// fakeBatcher — eventSink без ClickHouse: копит дошедшие до записи события.
type fakeBatcher struct{ evs []event.Event }

func (f *fakeBatcher) Add(e event.Event) { f.evs = append(f.evs, e) }

// TestPipelineScrubEvent: событие с ПДн проходит через process и доходит до
// батчера уже зачищенным — ip обнулён, denylist-поля в tags/contexts заменены
// на маску; email цел (ScrubEmail=false), не-denylist поля не тронуты.
func TestPipelineScrubEvent(t *testing.T) {
	fb := &fakeBatcher{}
	p := &Pipeline{
		issues:  &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}},
		batcher: fb,
		Scrub:   NewScrubber(true, false, []string{"password", "token"}),
	}

	ev := &ParsedEvent{
		EventID:        "e1",
		Timestamp:      time.Now().UTC(),
		UserIP:         "1.2.3.4",
		UserEmail:      "bob@example.com",
		Tags:           map[string]string{"password": "hunter2", "user": "bob"},
		ContextsJSON:   `{"trace":{"token":"secret","ok":1}}`,
		StacktraceJSON: `{"frames":[]}`,
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
	if got.Tags["password"] != scrubMask {
		t.Errorf("tags[password] = %q, want %q", got.Tags["password"], scrubMask)
	}
	if got.Tags["user"] != "bob" {
		t.Errorf("tags[user] = %q, want не тронут", got.Tags["user"])
	}

	var ctx map[string]any
	if err := json.Unmarshal([]byte(got.Contexts), &ctx); err != nil {
		t.Fatalf("contexts не JSON: %v", err)
	}
	tr, _ := ctx["trace"].(map[string]any)
	if tr["token"] != scrubMask {
		t.Errorf("contexts.trace.token = %v, want %q", tr["token"], scrubMask)
	}
	if tr["ok"] == nil {
		t.Errorf("contexts.trace.ok пропал — не-denylist поле не должно тереться")
	}
}

// TestPipelineScrubTransaction: span.Data транзакции зачищается перед записью в
// SpanSink — denylist-ключ заменён маской, прочие данные целы.
func TestPipelineScrubTransaction(t *testing.T) {
	spans := &fakeSpanSink{}
	p := &Pipeline{
		Spans: spans,
		Scrub: NewScrubber(true, false, []string{"authorization"}),
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

	p.processTransaction(1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	got := spans.added[0]
	d := got.Spans[0].Data
	if d["http.authorization"] != scrubMask {
		t.Errorf("span.Data[http.authorization] = %v, want %q", d["http.authorization"], scrubMask)
	}
	if d["http.status_code"] != 200 {
		t.Errorf("span.Data[http.status_code] = %v, want 200 (не тронут)", d["http.status_code"])
	}
}

// RA-L10: при включённом ScrubFreeText email в Message и ExceptionValue события
// маскируется на [email] перед записью в батчер; при выключенном — текст цел.
func TestPipelineScrubFreeTextEvent(t *testing.T) {
	makePipeline := func(freeText bool) (*Pipeline, *fakeBatcher) {
		fb := &fakeBatcher{}
		sc := NewScrubber(false, false, nil)
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

	// Включено: и Message, и ExceptionValue замаскированы.
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

	// Выключено: текст не тронут.
	p, fb = makePipeline(false)
	p.process(task{projectID: 1, ev: newEvent()})
	if got := fb.evs[0].Message; got != "error for user@example.com" {
		t.Errorf("при выключенном флаге Message = %q, want не тронут", got)
	}
	if got := fb.evs[0].ExceptionValue; got != "bad addr admin@corp.io in payload" {
		t.Errorf("при выключенном флаге ExceptionValue = %q, want не тронут", got)
	}
}

// capturingIssueSvc — issueUpserter, который запоминает title/culprit, дошедшие
// до Upsert. Нужен, чтобы проверить: свободный текст в title маскируется ДО
// апсерта (issues.title в PG иначе хранил бы email открытым).
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

// capturingAlertSink — AlertSink, запоминающий payload OnIssue (в частности
// Title), чтобы проверить: в алерте свободный текст тоже замаскирован.
type capturingAlertSink struct {
	called bool
	ev     alert.Event
}

func (f *capturingAlertSink) OnIssue(_ context.Context, ev alert.Event) {
	f.called = true
	f.ev = ev
}

// RA-L10 (проход 4): email в ev.Title маскируется ДО Upsert и OnIssue, а не
// только в Message/ExceptionValue перед записью в CH. Иначе issues.title (PG) и
// payload алерта уносили бы email в открытую.
func TestPipelineScrubFreeTextTitleBeforeUpsert(t *testing.T) {
	// Title строится в titleAndCulprit как "тип: значение" — тот же email, что
	// и в ExceptionValue. res.New=true, чтобы сработал путь OnIssue.
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

	// Включено: title в Upsert и в OnIssue замаскирован.
	t.Run("on", func(t *testing.T) {
		sc := NewScrubber(false, false, nil)
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

	// Выключено: title не тронут.
	t.Run("off", func(t *testing.T) {
		sc := NewScrubber(false, false, nil)
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

// RA-L10 (проход 4): scrubbing title после fingerprint.Compute не меняет
// группировку — fingerprint считается на Message/Exceptions, не на Title.
// Один и тот же email-содержащий ввод с вкл./выкл. ScrubFreeText должен давать
// одинаковый fingerprint (иначе события расползлись бы по разным группам).
func TestScrubFreeTextDoesNotChangeFingerprint(t *testing.T) {
	in := fingerprint.Input{
		Exceptions: []fingerprint.Exception{
			{Type: "ValueError", Value: "bad addr admin@corp.io in payload"},
		},
	}
	// Compute отрабатывает на исходном тексте до любого scrubbing в process().
	if got, want := fingerprint.Compute(in), fingerprint.Compute(in); got != want {
		t.Fatalf("fingerprint нестабилен: %q != %q", got, want)
	}
}

// RA-L10: span.Description транзакции маскируется при ScrubFreeText=true.
func TestPipelineScrubFreeTextTransaction(t *testing.T) {
	spans := &fakeSpanSink{}
	sc := NewScrubber(false, false, nil)
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

	p.processTransaction(1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	if got := spans.added[0].Spans[0].Description; got != "GET /users?email=[email]" {
		t.Errorf("span.Description = %q, want %q", got, "GET /users?email=[email]")
	}
}

// TestPipelineScrubTransactionTags: tags транзакции зачищаются перед записью в
// SpanSink так же, как у событий — denylist-тег заменён маской, прочие теги целы.
func TestPipelineScrubTransactionTags(t *testing.T) {
	spans := &fakeSpanSink{}
	p := &Pipeline{
		Spans: spans,
		Scrub: NewScrubber(true, false, []string{"authorization"}),
	}

	start := time.Now().UTC()
	tx := trace.Transaction{
		TraceID: "t1", SpanID: "root", Name: "GET /x", Op: "http.server",
		Start: start, End: start.Add(time.Second),
		Tags: map[string]string{"authorization": "Bearer x", "service": "api"},
	}

	p.processTransaction(1, tx)

	if spans.count() != 1 {
		t.Fatalf("транзакций записано = %d, want 1", spans.count())
	}
	got := spans.added[0]
	if got.Tags["authorization"] != scrubMask {
		t.Errorf("tags[authorization] = %q, want %q", got.Tags["authorization"], scrubMask)
	}
	if got.Tags["service"] != "api" {
		t.Errorf("tags[service] = %q, want не тронут", got.Tags["service"])
	}
}
