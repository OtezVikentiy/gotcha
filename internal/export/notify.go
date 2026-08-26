package export

import (
	"context"
	"fmt"
	"log/slog"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Mailer — минимальный контракт отправки письма, которого хватает
// нотифаеру выгрузок. Отдельный интерфейс (не notify.Sender целиком) —
// пакет не должен тянуть весь internal/notify ради одной сигнатуры; сама
// сигнатура намеренно совпадает с notify.Sender, так что *notify.EmailSender
// (тот же, что шлёт приглашения в организацию) подставляется без адаптера.
type Mailer interface {
	Send(ctx context.Context, t notify.Target, payload map[string]any) error
}

// NewMailNotifier собирает функцию для поля Worker.Notify: письмо автору
// заявки о готовности либо неудаче выгрузки — по образцу приглашения в
// организацию (internal/web/orgsettings.go, orgSettingsInvite). m == nil
// (почта не настроена) — тихо ничего не делать: файл уже на диске, письмо
// вторично, заявка остаётся успешной без него.
//
// locale — тем же способом, что и остальные фоновые нотифаеры продукта
// (internal/alert/digest.go, Digester.Locale): у воркера нет запроса, а
// значит нет и языка получателя, поэтому язык письма — локаль ИНСТАНСА
// (GOTCHA_LOCALE), переданная явным полем, а не прочитанная из ctx вызова.
// ctx, который Worker передаёт в Notify, — это ctx фонового цикла (в
// проде — тот, что main.go отдаёт Worker.Run), и локали не несёт; читать
// её оттуда означало бы тихо всегда попадать на i18n.Default. Второй способ
// (угадывать язык по адресату) развёл бы язык этого письма с языком
// остальных писем продукта.
func NewMailNotifier(m Mailer, st *Store, baseURL string, locale i18n.Locale) func(context.Context, Job) {
	return func(ctx context.Context, job Job) {
		if m == nil {
			return
		}
		ctx = i18n.WithLocale(ctx, locale)
		payload, ok := mailPayload(ctx, job, baseURL)
		if !ok {
			// Статус не терминальный — Worker такой снимок в проде не
			// присылает, но notify.go не должен ни упасть, ни отправить
			// письмо не по делу, если всё же пришло что-то ещё.
			return
		}
		email, err := st.AuthorEmail(ctx, job.CreatedBy)
		if err != nil {
			// Автора могло не оказаться (аккаунт удалён гонкой между
			// постановкой заявки и её завершением) — письмо не критично,
			// файл всё равно ждёт на странице выгрузок для тех, у кого
			// есть доступ к проекту.
			slog.Warn("export: письмо об итоге заявки: адрес автора", "job_id", job.ID, "err", err)
			return
		}
		if err := m.Send(ctx, notify.Target{Kind: "email", Target: email}, payload); err != nil {
			slog.Warn("export: письмо об итоге заявки: отправка", "job_id", job.ID, "err", err)
		}
	}
}

// mailPayload собирает тему и тело письма по статусу заявки. ok=false —
// статус не Done/Failed, письма для него не предусмотрено.
func mailPayload(ctx context.Context, job Job, baseURL string) (map[string]any, bool) {
	link := fmt.Sprintf("%s/projects/%d/exports", baseURL, job.ProjectID)
	switch job.Status {
	case StatusDone:
		body := i18n.Tf(ctx, "exports.mail.done.body", "link", link)
		if job.Truncated {
			body += " " + i18n.T(ctx, "exports.mail.truncated_note")
		}
		return map[string]any{
			"subject": i18n.T(ctx, "exports.mail.done.subject"),
			"body":    body,
		}, true
	case StatusFailed:
		// job.LastError — техническая диагностика (обход бага драйвера,
		// путь на диске, текст стандартной библиотеки) для БД и лога, не
		// для письма: раньше сюда шла она напрямую, и англоязычный автор
		// получал русский обрывок вперемешку с переводом (находка задачи 14
		// «долг гейтов E1», TestNoCyrillicUserFacingLiterals). {cause}
		// собирается из FailureReasonKey — переведённой причины, которую
		// расставляет Worker.notifyFailed (см. reasonDiskFull и соседние
		// константы в worker.go); пустой ключ — защита на случай снимка,
		// собранного не через fail()/failPermanent (в проде такого не
		// бывает, см. их докблоки), а не ожидаемый путь.
		reasonKey := job.FailureReasonKey
		if reasonKey == "" {
			reasonKey = reasonInternal
		}
		return map[string]any{
			"subject": i18n.T(ctx, "exports.mail.failed.subject"),
			"body":    i18n.Tf(ctx, "exports.mail.failed.body", "cause", i18n.T(ctx, reasonKey), "link", link),
		}, true
	default:
		return nil, false
	}
}
