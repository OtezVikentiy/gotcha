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

// mailCall — один зафиксированный вызов fakeMailer.Send.
type mailCall struct {
	target  notify.Target
	payload map[string]any
}

// fakeMailer подменяет реальный SMTP: тестам notify.go нужен факт и
// содержимое письма, а не его доставка.
type fakeMailer struct {
	calls []mailCall
	err   error
	// lastCtx — ctx, с которым пришёл ПОСЛЕДНИЙ вызов Send: нужен только
	// TestMailNotifierSendHasOwnTimeout (P2-OPS-3 аудита), проверяющей, что
	// NewMailNotifier не пробрасывает родительский ctx как есть.
	lastCtx context.Context
}

func (m *fakeMailer) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	m.calls = append(m.calls, mailCall{target: t, payload: payload})
	m.lastCtx = ctx
	return m.err
}

// mailBody достаёт тело письма из payload — payload собирается через
// map[string]any (см. образец в orgsettings.go), поэтому значение — string,
// но приводим через fmt.Sprint, чтобы неверный тип провалил ассерт текстом,
// а не паникой на приведении типа.
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
	// §9 спеки: письмо об успехе несёт число строк и размер файла, не
	// только ссылку — RowsWritten=10, Bytes=1000 в снимке заявки выше.
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

// TestMailNotifierSendHasOwnTimeout — P2-OPS-3 аудита: раньше m.Send
// получал jobCtx воркера как есть (живёт до Config.JobTimeout, по
// умолчанию 15 минут, без собственного дедлайна у DialContext) — зависший
// SMTP держал бы advisory lock воркера все эти 15 минут. ctx, дошедший до
// Mailer.Send, обязан нести СВОЙ, более короткий дедлайн независимо от
// родительского ctx (здесь — вовсе без дедлайна, context.Background()).
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

// TestMailNotifierReportsFailureCause — письмо о неудаче показывает
// ПЕРЕВЕДЁННУЮ причину (FailureReasonKey), а не техническую строку
// LastError дословно: до задачи 14 «долг гейтов E1» тело письма собиралось
// прямо из job.LastError — русский текст попадал в письмо даже на
// английской локали (находка TestNoCyrillicUserFacingLiterals). LastError
// здесь намеренно НЕ входит в переведённую причину (реалистичная
// диагностика: путь на диске, код ошибки ОС), чтобы мутация "тело
// собирается из LastError, а не из FailureReasonKey" ловилась явно.
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

// TestMailNotifierFallsBackToInternalReasonWhenKeyMissing — снимок Job без
// FailureReasonKey (в проде такого не бывает: notifyFailed в worker.go
// всегда его проставляет, см. её докблок) не должен собрать пустую причину
// в письме — mailPayload обязана подставить reasonInternal защитно.
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

	// Почта не настроена — заявка всё равно считается успешной: файл уже
	// на диске. NewMailNotifier(nil, ...) не должен паниковать.
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
	// CreatedBy указывает на несуществующего пользователя (аккаунт мог быть
	// удалён между постановкой заявки и её завершением): AuthorEmail не
	// находит адрес, письмо тихо не уходит, паники нет.
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
	// Воркер зовёт Notify только при завершении заявки; queued/running сюда
	// в проде не долетают, но notify.go не должен упасть или отправить
	// письмо не по адресу, если снимок заявки всё же не терминальный.
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
	// Ошибка отправки — best-effort: файл уже собран, письмо вторично, и
	// её сбой не должен всплыть наружу как паника или "перезаявка".
	notifyFn(ctx, Job{ID: 8, ProjectID: projectID, CreatedBy: userID, Status: StatusDone})
}

// TestMailNotifierUsesConfiguredLocale — язык письма берётся из locale,
// переданной в NewMailNotifier (локаль ИНСТАНСА, как у Digester.Locale в
// internal/alert/digest.go), а не из того, что случайно лежит в ctx
// вызова: у Worker.Run в проде locale в ctx нет вовсе (см. комментарий
// NewMailNotifier). Здесь же нарочно кладём в ctx ДРУГУЮ локаль, чтобы
// доказать, что notify.go её игнорирует и переопределяет своей.
func TestMailNotifierUsesConfiguredLocale(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	// ctx несёт "en" — locale, переданная в NewMailNotifier, несёт "ru".
	// Победить обязана configured-локаль.
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

// TestMailNotifierUsesConfiguredLocaleEmptyContext — тот же контракт, но
// с "пустым" ctx (как реально приходит в проде из Worker.Run): без явной
// locale i18n.FromContext молча откатился бы на i18n.Default ("ru"),
// поэтому этот тест дублирует смысл предыдущего только частично — берём EN,
// чтобы отличить "сработала configured-локаль" от "совпало с дефолтом".
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

// TestMailNotifierIncludesMachineReadableMeta — F5 контрактной уборки
// 2026-08-28 (CONTRACT-DECISIONS.md): письмо о готовности обязано нести
// job_id/scope_issue_id/filter_code машиночитаемо, рядом с локализованной
// фразой, а не только внутри неё — получателю не приходится парсить текст
// вида «issue #77» на языке инстанса, чтобы достать число 77.
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

// TestMailNotifierIncludesPseudonymNoteForMaskedEvents — F1′: письмо о
// готовности обязано нести пометку о невозможности сопоставить псевдонимы
// user_id между выгрузками РОВНО там, где BuildMeta её ставит (Kind=events,
// IncludePII=false) — тот же контракт, что и у Meta.PseudonymNote (meta.go,
// см. TestBuildMetaPseudonymNoteOnlyForMaskedEvents).
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

// TestMailNotifierOmitsPseudonymNoteWhenNotMasked — зеркало предыдущего
// теста: пометка не появляется там, где псевдонимизации нет вовсе (issues —
// колонки user_id нет) или PII отдан сырым (IncludePII=true) — предупреждать
// о свойстве, которого нет, было бы ложью получателю письма.
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
