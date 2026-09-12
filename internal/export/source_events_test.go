package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestEventColumnsContractPin(t *testing.T) {
	want := []string{"timestamp", "event_id", "issue_id", "level", "message",
		"exception_type", "exception_value", "environment", "release", "server_name",
		"sdk", "trace_id", "user_id", "user_ip", "user_email", "tags"}
	got := EventColumns()
	if len(got) != len(want) {
		t.Fatalf("EventColumns() = %v (%d колонок), want %v (%d) — контракт CSV изменился без записи в CHANGELOG?", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EventColumns()[%d] = %q, want %q — переименование/перестановка публичной колонки требует записи в CHANGELOG", i, got[i], want[i])
		}
	}
}

func seedEvent(t *testing.T, b *event.Batcher, ev event.Event) {
	t.Helper()
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.Level == "" {
		ev.Level = issue.LevelError
	}
	b.Add(ev)
}

func TestEventSourcePipelineThroughWriter(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-events", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	requestJSON := `{"method":"GET","url":"https://x/","headers":{"Authorization":"Bearer secret-abc","token":"tok-123"}}`
	contextsJSON := `{"auth":{"token":"ctx-tok-456"}}`

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", ExceptionType: "ValueError", ExceptionValue: "bad",
		Environment: "prod", Release: "1.0", ServerName: "web-1", SDK: "sentry.go/0.x",
		UserID: "u1", UserIP: "203.0.113.7", UserEmail: "user@example.com",
		Tags:     map[string]string{"b": "2", "a": "1"},
		Contexts: contextsJSON, Request: requestJSON, Stacktrace: `{"values":[{"type":"ValueError"}]}`,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Run("маска включена по умолчанию", func(t *testing.T) {
		src := NewEventSource(event.NewQuery(ch), svc)
		var buf bytes.Buffer
		w, err := NewWriter(&buf, FormatNDJSON, EventColumns())
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := src.Stream(ctx, projectID, 0, false /*includePII*/, Params{}, func(r Record) error {
			return w.Write(r)
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		out := buf.String()
		for _, leak := range []string{"secret-abc", "tok-123", "ctx-tok-456"} {
			if strings.Contains(out, leak) {
				t.Errorf("секрет %q утёк в выгрузку: %s", leak, out)
			}
		}
		var row map[string]any
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("NDJSON: %d строк, want 1: %q", len(lines), out)
		}
		if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if row["user_ip"] != maskedValue || row["user_email"] != maskedValue {
			t.Errorf("PII не замаскированы: user_ip=%v user_email=%v", row["user_ip"], row["user_email"])
		}
		uid, _ := row["user_id"].(string)
		if uid == "" || uid == "u1" {
			t.Errorf("user_id = %v, want непустой псевдоним, отличный от исходного u1", row["user_id"])
		}
		if row["issue_id"] != float64(res.IssueID) {
			t.Errorf("issue_id = %v, want %d", row["issue_id"], res.IssueID)
		}
		if row["tags"] != "a=1; b=2" {
			t.Errorf("tags = %v, want отсортированную плоскую строку", row["tags"])
		}
	})

	t.Run("включить как есть — includePII", func(t *testing.T) {
		src := NewEventSource(event.NewQuery(ch), svc)
		var buf bytes.Buffer
		w, err := NewWriter(&buf, FormatNDJSON, EventColumns())
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := src.Stream(ctx, projectID, 0, true /*includePII*/, Params{}, func(r Record) error {
			return w.Write(r)
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		out := buf.String()
		for _, want := range []string{"secret-abc", "tok-123", "ctx-tok-456", "203.0.113.7", "user@example.com"} {
			if !strings.Contains(out, want) {
				t.Errorf("includePII=true обязан отдавать %q как есть, не нашёл в: %s", want, out)
			}
		}
	})
}

func TestEventSourceMasksPIICarriedInTags(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-tags-pii", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", Environment: "prod",
		Tags: map[string]string{"user.email": "victim@example.com", "env": "prod"},
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)

	t.Run("маска включена по умолчанию — тег с email тоже скрыт", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, false /*includePII*/, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		tags, _ := got["tags"].(string)
		if strings.Contains(tags, "victim@example.com") {
			t.Errorf("email утёк через tags при includePII=false: %s", tags)
		}
		if !strings.Contains(tags, "env=prod") {
			t.Errorf("безобидный тег потерян: %s", tags)
		}
	})

	t.Run("includePII=true — тег как есть", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, true /*includePII*/, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		tags, _ := got["tags"].(string)
		if !strings.Contains(tags, "victim@example.com") {
			t.Errorf("includePII=true обязан отдавать тег как есть, не нашёл email: %s", tags)
		}
	})
}

func TestEventSourceCSVOmitsRawFields(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)
	seenAt := time.Now().UTC()
	res, err := svc.Upsert(ctx, projectID, "fp-csv", "boom", "", issue.LevelError, "", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", Stacktrace: `{"values":["уникальный-маркер-стека"]}`,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatCSV, EventColumns())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := src.Stream(ctx, projectID, 0, true, Params{}, func(r Record) error { return w.Write(r) }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(buf.String(), "уникальный-маркер-стека") {
		t.Errorf("stacktrace попал в CSV: %s", buf.String())
	}
}

func TestEventSourceScopeSingleIssue(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)
	seenAt := time.Now().UTC()
	target, err := svc.Upsert(ctx, projectID, "fp-target", "target", "", issue.LevelError, "", seenAt)
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	other, err := svc.Upsert(ctx, projectID, "fp-other", "other", "", issue.LevelError, "", seenAt)
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{ProjectID: projectID, IssueID: target.IssueID, Timestamp: seenAt, Message: "target"})
	seedEvent(t, b, event.Event{ProjectID: projectID, IssueID: other.IssueID, Timestamp: seenAt, Message: "other"})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)
	var messages []string
	err = src.Stream(ctx, projectID, target.IssueID, true, Params{}, func(r Record) error {
		messages = append(messages, r["message"].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(messages) != 1 || messages[0] != "target" {
		t.Fatalf("Stream(scope=target) = %v, want ровно [target]", messages)
	}
}

func TestEventSourceEmptyFilterYieldsNoRecordsWithoutError(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)
	if _, err := svc.Upsert(ctx, projectID, "fp-empty", "t", "", issue.LevelError, "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)
	calls := 0
	err := src.Stream(ctx, projectID, 0, true, Params{Status: "resolved"}, func(Record) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if calls != 0 {
		t.Errorf("колбэк вызван %d раз при пустом наборе групп", calls)
	}
}

func TestEventSourceIsolatedByProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectA, _ := seedProjectAndUser(t, pool)
	projectB, _ := seedProjectAndUser(t, pool)
	seenAt := time.Now().UTC()

	issueA, err := svc.Upsert(ctx, projectA, "fp-a", "own", "", issue.LevelError, "", seenAt)
	if err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	issueB, err := svc.Upsert(ctx, projectB, "fp-b", "foreign", "", issue.LevelError, "", seenAt)
	if err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{ProjectID: projectA, IssueID: issueA.IssueID, Timestamp: seenAt, Message: "own"})
	seedEvent(t, b, event.Event{ProjectID: projectB, IssueID: issueB.IssueID, Timestamp: seenAt, Message: "foreign"})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)

	t.Run("фильтр по проекту", func(t *testing.T) {
		var messages []string
		if err := src.Stream(ctx, projectA, 0, true, Params{}, func(r Record) error {
			messages = append(messages, r["message"].(string))
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if len(messages) != 1 || messages[0] != "own" {
			t.Fatalf("Stream(projectA) = %v, чужое событие проекта B утекло", messages)
		}
	})

	t.Run("чужой ScopeIssueID не утекает", func(t *testing.T) {
		var messages []string
		if err := src.Stream(ctx, projectA, issueB.IssueID, true, Params{}, func(r Record) error {
			messages = append(messages, r["message"].(string))
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if len(messages) != 0 {
			t.Fatalf("Stream(projectA, scope=чужой issue) = %v, want пусто", messages)
		}
	})
}

func TestEventSourceTooManyIssuesReturnsError(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	for i := 0; i < 3; i++ {
		if _, err := svc.Upsert(ctx, projectID, "fp-of-"+string(rune('a'+i)), "t", "", issue.LevelError, "", time.Now().UTC()); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	src := &eventSource{q: event.NewQuery(ch), issues: svc, maxIssueIDs: 2}
	err := src.Stream(ctx, projectID, 0, true, Params{}, func(Record) error {
		t.Fatal("колбэк не должен вызываться при отказе на потолке")
		return nil
	})
	if err == nil {
		t.Fatal("Stream не вернул ошибку при упоре в потолок id групп")
	}
	if !errors.Is(err, ErrTooManyIssues) {
		t.Fatalf("err = %v, want ErrTooManyIssues", err)
	}
}

func TestEventSourceZeroMaxIssueIDsFailsCleanly(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)
	if _, err := svc.Upsert(ctx, projectID, "fp-zero", "t", "", issue.LevelError, "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src := &eventSource{q: event.NewQuery(ch), issues: svc} // maxIssueIDs не задан — 0
	err := src.Stream(ctx, projectID, 0, true, Params{}, func(Record) error {
		t.Fatal("колбэк не должен вызываться при незаданном потолке")
		return nil
	})
	if err == nil {
		t.Fatal("Stream не вернул ошибку при нулевом maxIssueIDs")
	}
	if errors.Is(err, ErrTooManyIssues) {
		t.Fatalf("err = %v — ошибка конструирования замаскирована под ErrTooManyIssues", err)
	}
	if !errors.Is(err, ErrMaxIssueIDsNotConfigured) {
		t.Fatalf("err = %v, want ErrMaxIssueIDsNotConfigured", err)
	}
}

func TestNewEventSourceDefaultsMaxIssueIDs(t *testing.T) {
	src, ok := NewEventSource(nil, nil).(*eventSource)
	if !ok {
		t.Fatalf("NewEventSource вернул %T, want *eventSource", src)
	}
	if src.maxIssueIDs != defaultMaxIssueIDsForEventExport {
		t.Fatalf("maxIssueIDs = %d, want %d (дефолт из NewEventSource)", src.maxIssueIDs, defaultMaxIssueIDsForEventExport)
	}
}

func TestEventSourceMasksStacktraceAndBreadcrumbs(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-stack-pii", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	stacktraceJSON := `{"values":[{"type":"ValueError","frames":[{"vars":{"auth_token":"stk-tok-789","user_note":"contact victim@example.com"}}]}]}`
	breadcrumbsJSON := `{"values":[{"type":"http","message":"GET https://api.example.com/x?token=brc-tok-321"}]}`

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", Environment: "prod",
		Stacktrace: stacktraceJSON, Breadcrumbs: breadcrumbsJSON,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)

	t.Run("includePII=false — ПДн вычищены из обоих полей", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, false, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		stackOut, _ := json.Marshal(got["stacktrace"])
		brcOut, _ := json.Marshal(got["breadcrumbs"])
		for _, leak := range []string{"stk-tok-789", "victim@example.com", "brc-tok-321"} {
			if strings.Contains(string(stackOut), leak) || strings.Contains(string(brcOut), leak) {
				t.Errorf("ПДн %q утекли при includePII=false: stacktrace=%s breadcrumbs=%s", leak, stackOut, brcOut)
			}
		}
	})

	t.Run("includePII=true — оба поля как есть", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, true, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		stackOut, _ := json.Marshal(got["stacktrace"])
		brcOut, _ := json.Marshal(got["breadcrumbs"])
		for _, want := range []string{"stk-tok-789", "victim@example.com", "brc-tok-321"} {
			if !strings.Contains(string(stackOut), want) && !strings.Contains(string(brcOut), want) {
				t.Errorf("includePII=true обязан отдать %q как есть: stacktrace=%s breadcrumbs=%s", want, stackOut, brcOut)
			}
		}
	})
}

func TestEventSourceMasksMessageAndExceptionValue(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-msg-pii", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message:        "failed request for victim@example.com",
		ExceptionValue: "GET https://api.example.com/x?token=msg-tok-654 timed out",
		Environment:    "prod",
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)

	t.Run("includePII=false — email и токен вычищены", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, false, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		msg, _ := got["message"].(string)
		exc, _ := got["exception_value"].(string)
		if strings.Contains(msg, "victim@example.com") {
			t.Errorf("message = %q, email не вычищен", msg)
		}
		if strings.Contains(exc, "msg-tok-654") {
			t.Errorf("exception_value = %q, токен не вычищен", exc)
		}
	})

	t.Run("includePII=true — как есть", func(t *testing.T) {
		var got Record
		if err := src.Stream(ctx, projectID, 0, true, Params{}, func(r Record) error {
			got = r
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		msg, _ := got["message"].(string)
		exc, _ := got["exception_value"].(string)
		if !strings.Contains(msg, "victim@example.com") {
			t.Errorf("message = %q, includePII=true обязан отдать email как есть", msg)
		}
		if !strings.Contains(exc, "msg-tok-654") {
			t.Errorf("exception_value = %q, includePII=true обязан отдать токен как есть", exc)
		}
	})
}

func TestEventSourceUserIDPseudonym(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-uid-pii", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", Environment: "prod", UserID: "alice@example.com",
	})
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt.Add(time.Second),
		Message: "boom", Environment: "prod", UserID: "alice@example.com",
	})
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt.Add(2 * time.Second),
		Message: "boom", Environment: "prod", UserID: "bob@example.com",
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)
	var uids []string
	if err := src.Stream(ctx, projectID, 0, false, Params{}, func(r Record) error {
		uid, _ := r["user_id"].(string)
		uids = append(uids, uid)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(uids) != 3 {
		t.Fatalf("получено %d записей, want 3: %v", len(uids), uids)
	}
	for _, uid := range uids {
		if uid == "" {
			t.Fatalf("пустой псевдоним в выдаче: %v", uids)
		}
		if uid == "alice@example.com" || uid == "bob@example.com" {
			t.Fatalf("user_id уехал сырым: %v", uids)
		}
	}
	counts := map[string]int{}
	for _, uid := range uids {
		counts[uid]++
	}
	if len(counts) != 2 {
		t.Fatalf("различных псевдонимов %d, want 2 (alice/bob): %v", len(counts), uids)
	}
	var pairCount, soloCount int
	for _, n := range counts {
		switch n {
		case 2:
			pairCount++
		case 1:
			soloCount++
		}
	}
	if pairCount != 1 || soloCount != 1 {
		t.Errorf("распределение псевдонимов = %v, want один псевдоним дважды (alice) и один — один раз (bob)", counts)
	}
}

func TestEventSourceUserIDPseudonymDiffersAcrossExports(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-uid-cross-export", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	seedEvent(t, b, event.Event{
		ProjectID: projectID, IssueID: res.IssueID, Timestamp: seenAt,
		Message: "boom", Environment: "prod", UserID: "carol@example.com",
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewEventSource(event.NewQuery(ch), svc)

	runOnce := func() string {
		var uid string
		if err := src.Stream(ctx, projectID, 0, false, Params{}, func(r Record) error {
			uid, _ = r["user_id"].(string)
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if uid == "" {
			t.Fatalf("пустой псевдоним user_id")
		}
		return uid
	}

	first := runOnce()
	second := runOnce()

	if first == "carol@example.com" || second == "carol@example.com" {
		t.Fatalf("user_id уехал сырым: first=%q, second=%q", first, second)
	}
	if first == second {
		t.Fatalf("псевдоним user_id совпал между ДВУМЯ разными выгрузками (first=%q, second=%q) — salt пережил Stream, две выгрузки стали сопоставимы по user_id", first, second)
	}
}
