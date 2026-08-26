package export

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
}

func (m *fakeMailer) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	m.calls = append(m.calls, mailCall{target: t, payload: payload})
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
	const cause = "источник событий недоступен: контекст истёк"
	notifyFn(ctx, Job{
		ID: 4, ProjectID: projectID, CreatedBy: userID,
		Status: StatusFailed, LastError: cause,
	})

	if len(sent.calls) != 1 {
		t.Fatalf("отправлено писем: %d, ожидали 1", len(sent.calls))
	}
	body := mailBody(sent.calls[0])
	if !strings.Contains(body, cause) {
		t.Errorf("причина отказа не попала в письмо: %q", body)
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
