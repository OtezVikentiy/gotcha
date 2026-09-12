package export

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type mailCall struct {
	target  notify.Target
	payload map[string]any
}

type fakeMailer struct {
	calls []mailCall
	err   error
	// Нужен только тесту таймаута — проверить, что не переиспользуется родительский ctx.
	lastCtx context.Context
}

func (m *fakeMailer) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	m.calls = append(m.calls, mailCall{target: t, payload: payload})
	m.lastCtx = ctx
	return m.err
}

// Приводим через fmt.Sprint, чтобы неверный тип провалил ассерт текстом, а не паникой.
func mailBody(c mailCall) string { return fmt.Sprint(c.payload["body"]) }

func TestMailNotifierReportsSuccessWithLink(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 1, ProjectID: projectID, CreatedBy: userID,
		Status: StatusDone, RowsWritten: 10, Bytes: 1000,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	if sent.calls[0].target.Kind != "email" || sent.calls[0].target.Target == "" {
		t.Errorf("адресат письма пуст или не email: %+v", sent.calls[0].target)
	}
	body := mailBody(sent.calls[0])
	wantLink := fmt.Sprintf("https://gotcha.example/projects/%d/exports", projectID)
	if !strings.Contains(body, wantLink) {
		t.Errorf("в письме нет ссылки на страницу выгрузок: %q", body)
	}
	if !strings.Contains(body, "10 строк") {
		t.Errorf("в письме нет числа строк выгрузки: %q", body)
	}
	if !strings.Contains(body, "1000B") {
		t.Errorf("в письме нет размера файла выгрузки: %q", body)
	}
	// Ссылки на сам файл в письме быть не должно: данные по почтовой
	// ссылке не отдаём, только через авторизованное скачивание со страницы.
	if strings.Contains(body, "/download") {
		t.Error("письмо ведёт прямо на файл")
	}
	subject := fmt.Sprint(sent.calls[0].payload["subject"])
	if subject == "" {
		t.Error("тема письма пуста")
	}
}

func TestMailNotifierSendHasOwnTimeout(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 1, ProjectID: projectID, CreatedBy: userID,
		Status: StatusDone, RowsWritten: 1, Bytes: 1,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	deadline, ok := sent.lastCtx.Deadline()
	if !ok {
		t.Fatal("ctx у Mailer.Send без дедлайна — родительский ctx проброшен как есть")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > sendTimeout {
		t.Errorf("дедлайн ctx = %s от текущего момента, want в (0, %s]", remaining, sendTimeout)
	}
}

func TestMailNotifierMentionsTruncation(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 2, ProjectID: projectID, CreatedBy: userID,
		Status: StatusDone, RowsWritten: 100, Bytes: 1 << 20, Truncated: true,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	if !strings.Contains(mailBody(sent.calls[0]), "обрезан") {
		t.Error("обрезка не упомянута — человек примет неполный файл за полный")
	}
}

func TestMailNotifierDoesNotMentionTruncationWhenNotTruncated(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 3, ProjectID: projectID, CreatedBy: userID,
		Status: StatusDone, RowsWritten: 1, Bytes: 10, Truncated: false,
	})

	if strings.Contains(mailBody(sent.calls[0]), "обрезан") {
		t.Error("файл не обрезан, а письмо утверждает обратное")
	}
}

func TestMailNotifierReportsFailureCause(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	const technicalDiagnostic = "open /var/lib/gotcha/exports/4.part: no space left on device"
	notifyFn(ctx, Job{
		ID: 4, ProjectID: projectID, CreatedBy: userID,
		Status: StatusFailed, LastError: technicalDiagnostic, FailureReasonKey: reasonDiskFull,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	body := mailBody(sent.calls[0])
	if !strings.Contains(body, "на диске выгрузок закончилось место") {
		t.Errorf("переведённая причина (reasonDiskFull) не попала в письмо: %q", body)
	}
	if strings.Contains(body, technicalDiagnostic) {
		t.Errorf("техническая диагностика LastError утекла в письмо дословно: %q", body)
	}
}

func TestMailNotifierFallsBackToInternalReasonWhenKeyMissing(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 8, ProjectID: projectID, CreatedBy: userID,
		Status: StatusFailed, LastError: "что угодно",
	})

	body := mailBody(sent.calls[0])
	if !strings.Contains(body, "внутренняя ошибка при сборке файла") {
		t.Errorf("пустой FailureReasonKey не подменён reasonInternal: %q", body)
	}
}

func TestMailNotifierSilentWhenMailerNil(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	notifyFn := NewMailNotifier(nil, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{ID: 5, ProjectID: projectID, CreatedBy: userID, Status: StatusDone})
}

func TestMailNotifierSkipsUnknownAuthor(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{ID: 6, ProjectID: projectID, CreatedBy: 9_999_999, Status: StatusDone})

	if len(sent.calls) != 0 {
		t.Fatalf("отправлено писем: %d, ожидали 0 — автор не найден", len(sent.calls))
	}
}

func TestMailNotifierIgnoresNonTerminalStatus(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{ID: 7, ProjectID: projectID, CreatedBy: userID, Status: StatusQueued})

	if len(sent.calls) != 0 {
		t.Fatalf("отправлено писем: %d, ожидали 0 — статус не терминальный", len(sent.calls))
	}
}

func TestMailNotifierSendErrorDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{err: fmt.Errorf("smtp: connection refused")}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{ID: 8, ProjectID: projectID, CreatedBy: userID, Status: StatusDone})
}

func TestMailNotifierUsesConfiguredLocale(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{ID: 9, ProjectID: projectID, CreatedBy: userID, Status: StatusDone})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	subject := fmt.Sprint(sent.calls[0].payload["subject"])
	if subject != "[Gotcha] Выгрузка готова" {
		t.Errorf("тема письма не на configured-локали (ru): %q", subject)
	}
}

func TestMailNotifierUsesConfiguredLocaleEmptyContext(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "en"})
	notifyFn(context.Background(), Job{ID: 10, ProjectID: projectID, CreatedBy: userID, Status: StatusDone})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	subject := fmt.Sprint(sent.calls[0].payload["subject"])
	if subject != "[Gotcha] Export is ready" {
		t.Errorf("тема письма не на configured-локали (en): %q", subject)
	}
}

func TestMailNotifierIncludesMachineReadableMeta(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 11, ProjectID: projectID, CreatedBy: userID,
		Kind: KindIssues, ScopeIssueID: 77, Status: StatusDone,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	body := mailBody(sent.calls[0])
	want := "gotcha-export-meta: job_id=11 scope_issue_id=77 filter_code=issue"
	if !strings.Contains(body, want) {
		t.Errorf("в письме нет строки машиночитаемых метаданных: want %q содержится в %q", want, body)
	}
}

func TestMailNotifierIncludesPseudonymNoteForMaskedEvents(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	sent := &fakeMailer{}
	notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
	notifyFn(ctx, Job{
		ID: 12, ProjectID: projectID, CreatedBy: userID,
		Kind: KindEvents, IncludePII: false, Status: StatusDone,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	body := mailBody(sent.calls[0])
	if !strings.Contains(body, PseudonymUniquenessNote) {
		t.Errorf("письмо о готовности не несёт пометку о псевдонимах (F1′): %q", body)
	}
}

func TestMailNotifierOmitsPseudonymNoteWhenNotMasked(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	cases := []Job{
		{ID: 13, Kind: KindEvents, IncludePII: true},
		{ID: 14, Kind: KindIssues, IncludePII: false},
	}
	for _, base := range cases {
		sent := &fakeMailer{}
		notifyFn := NewMailNotifier(sent, st, "https://gotcha.example", i18n.Locale{Code: "ru"})
		job := base
		job.ProjectID = projectID
		job.CreatedBy = userID
		job.Status = StatusDone
		notifyFn(ctx, job)

		if len(sent.calls) != 1 {
			t.Fatalf("job=%+v: отправлено писем: %d, ожидали 1", job, len(sent.calls))
		}
		body := mailBody(sent.calls[0])
		if strings.Contains(body, PseudonymUniquenessNote) {
			t.Errorf("job=%+v: письмо несёт пометку о псевдонимах, хотя маскирования user_id нет: %q", job, body)
		}
	}
}
