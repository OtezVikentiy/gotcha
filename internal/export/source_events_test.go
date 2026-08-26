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

// seedEvent — минимальный вставщик события для тестов источника: только
// поля, важные конкретному тесту, заполняются вызывающим кодом через ev.
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

// TestEventSourcePipelineThroughWriter — сценарный тест полного пайплайна
// источник → Record → Writer: изолированные юниты источника и писателя
// могли бы поодиночке быть исправны и разойтись на стыке (например, в
// имени или типе колонки), а полный прогон через NDJSON это ловит.
// Заодно проверяет ОБА режима маскирования на реальном выводе писателя,
// а не на Record напрямую: request и contexts содержат ключ "token" из
// ingest.DefaultDenyKeys, и тест смотрит его отсутствие в файле.
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
		// Реальный вывод писателя, не Record напрямую: денилист-ключ не должен
		// доехать до файла ни в каком виде — ни как значение, ни как секрет
		// заголовка.
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
		if row["user_id"] != "u1" {
			t.Errorf("user_id = %v, want u1 (внутренний id не маскируется)", row["user_id"])
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

// TestEventSourceCSVOmitsRawFields — CSV не должен содержать stacktrace/
// contexts/breadcrumbs/request: полотно JSON в ячейке нечитаемо (§6 спеки).
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

// TestEventSourceScopeSingleIssue — область «одна группа» (ScopeIssueID
// заявки) идёт прямо в CH-фильтр без похода в PG и без учёта Params: заявка
// на выгрузку одной группы уже сама себе фильтр.
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

// TestEventSourceEmptyFilterYieldsNoRecordsWithoutError — фильтр, которому
// не соответствует ни одна группа, не должен ни падать (issue_id IN () —
// частая причина ошибки драйвера ClickHouse на пустом списке), ни вызывать
// колбэк ни разу.
func TestEventSourceEmptyFilterYieldsNoRecordsWithoutError(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)
	// Группа есть, но unresolved — фильтр по status=resolved ничего не найдёт.
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

// TestEventSourceIsolatedByProject — заявка проекта A не должна видеть
// события проекта B ни через «одну группу», ни через фильтр по проекту:
// одна из немногих утечек, где ошибка означает чужие персональные данные
// в чужом файле.
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
		// projectA запрашивает выгрузку с ScopeIssueID группы проекта B:
		// WHERE project_id = ? в StreamForExport обязан отсечь эту группу,
		// даже если id формально существует (в другом проекте).
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

// TestEventSourceTooManyIssuesReturnsError — упор в потолок числа групп
// даёт отказ (ErrTooManyIssues), а не тихую обрезку выгрузки. Тест собирает
// eventSource напрямую (пакет export общий с неэкспортируемым типом) с
// заниженным maxIssueIDs — заводить в PG 20 000 живых групп ради потолка
// по умолчанию незачем, сама величина дефолта проверяется отдельным тестом.
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

// TestEventSourceZeroMaxIssueIDsFailsCleanly — eventSource, собранный в
// обход NewEventSource с нулевым maxIssueIDs, обязан отказать явной ошибкой
// конструирования, а не молча выдать её за ErrTooManyIssues: без guard'а
// IDsForFilter(..., 0) уходит в LIMIT 1, и ЛЮБОЙ непустой результат тут же
// читается как overflow=true — «есть хотя бы одна группа» превращается в
// «слишком много групп», хотя дело в забытом потолке, а не в фильтре.
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

// TestNewEventSourceDefaultsMaxIssueIDs — NewEventSource обязан проставлять
// потолок по умолчанию (§8 спеки: 20 000), а не оставлять поле нулевым:
// нулевой maxIssueIDs означал бы, что любой непустой список групп сразу
// считается переполнением.
func TestNewEventSourceDefaultsMaxIssueIDs(t *testing.T) {
	src, ok := NewEventSource(nil, nil).(*eventSource)
	if !ok {
		t.Fatalf("NewEventSource вернул %T, want *eventSource", src)
	}
	if src.maxIssueIDs != defaultMaxIssueIDsForEventExport {
		t.Fatalf("maxIssueIDs = %d, want %d (дефолт из NewEventSource)", src.maxIssueIDs, defaultMaxIssueIDsForEventExport)
	}
}
