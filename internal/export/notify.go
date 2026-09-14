package export

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Независим от jobCtx (живёт до JobTimeout, обычно 15 мин) — без него зависший
// SMTP держал бы advisory lock воркера все эти 15 минут.
const sendTimeout = 30 * time.Second

// Отдельный интерфейс, не notify.Sender целиком, но той же сигнатуры —
// *notify.EmailSender подставляется без адаптера.
type Mailer interface {
	Send(ctx context.Context, t notify.Target, payload map[string]any) error
}

// m == nil (почта не настроена) — тихо ничего не делать: файл уже на диске.
// instanceLocale — локаль инстанса (GOTCHA_LOCALE), фолбэк для тех, кто не выбирал
// личную locale явно; за письмо отвечает получатель (job.CreatedBy), не ctx фонового цикла.
func NewMailNotifier(m Mailer, st *Store, baseURL string, instanceLocale i18n.Locale) func(context.Context, Job) {
	return func(ctx context.Context, job Job) {
		if m == nil {
			return
		}
		locale := instanceLocale
		if code, err := st.AuthorLocale(ctx, job.CreatedBy); err == nil {
			if l, ok := i18n.Parse(code); ok {
				locale = l
			}
		}
		ctx = i18n.WithLocale(ctx, locale)
		payload, ok := mailPayload(ctx, job, baseURL)
		if !ok {
			return
		}
		email, err := st.AuthorEmail(ctx, job.CreatedBy)
		if err != nil {
			// Автора могло не оказаться (аккаунт удалён гонкой между
			// постановкой заявки и её завершением) — письмо не критично.
			slog.Warn("export: письмо об итоге заявки: адрес автора", "job_id", job.ID, "err", err)
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		defer cancel()
		if err := m.Send(sendCtx, notify.Target{Kind: "email", Target: email}, payload); err != nil {
			slog.Warn("export: письмо об итоге заявки: отправка", "job_id", job.ID, "err", err)
		}
	}
}

// ok=false — статус не Done/Failed, письма для него не предусмотрено.
func mailPayload(ctx context.Context, job Job, baseURL string) (map[string]any, bool) {
	link := fmt.Sprintf("%s/projects/%d/exports", baseURL, job.ProjectID)
	switch job.Status {
	case StatusDone:
		// RowsWritten/Bytes уже проставлены process() из writeResult до вызова Notify.
		body := i18n.Tf(ctx, "exports.mail.done.body",
			"link", link,
			"rows", i18n.Tn(ctx, "exports.mail.done.rows", int(job.RowsWritten)),
			"size", humanize.Bytes(job.Bytes))
		if job.Truncated {
			body += " " + i18n.T(ctx, "exports.mail.truncated_note")
		}
		if job.ExpiresAt != nil {
			body += " " + i18n.Tf(ctx, "exports.mail.expires_note",
				"expires", humanize.Duration(ctx, time.Until(*job.ExpiresAt)))
		}
		// Та же непереведённая строка, что несёт Meta.PseudonymNote — контент файла не локализуется.
		meta := BuildMeta(job)
		if meta.PseudonymNote != "" {
			body += " " + meta.PseudonymNote
		}
		// Нелокализуемый префикс gotcha-export-meta: даёт вытащить
		// job_id/scope_issue_id/filter_code без разбора переведённого текста.
		body += "\n\n" + fmt.Sprintf("gotcha-export-meta: job_id=%d scope_issue_id=%d filter_code=%s",
			job.ID, meta.ScopeIssueID, meta.FilterCode)
		return map[string]any{
			"subject": i18n.T(ctx, "exports.mail.done.subject"),
			"body":    body,
		}, true
	case StatusFailed:
		// LastError — для БД/лога, не для письма (иначе смесь языков); {cause} — из
		// FailureReasonKey. Пустой ключ — защита на случай снимка не через fail()/failPermanent.
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
