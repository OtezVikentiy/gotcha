// Package flashctx — одноразовое сообщение о результате действия в контексте
// запроса.
//
// Отдельный листовой пакет, а не часть web, по той же причине, что i18n и
// theme: его читает слой шаблонов (internal/web/templates), а тот импортируется
// самим web — обратный импорт замкнул бы цикл.
package flashctx

import "context"

// Flash — сообщение после действия: «сохранено», «удалено N записей».
type Flash struct {
	// Kind — "ok" или "warn". Влияет только на оформление.
	Kind string
	// Key — i18n-ключ сообщения. Хранится КЛЮЧ, а не текст: сообщение
	// переносится через cookie, которую ставит клиент, и произвольный текст
	// оттуда был бы готовой площадкой для фишинга на нашей же странице.
	Key string
	// N — число для форм множественного числа (i18n.Tn). 0 → обычный перевод.
	N int
}

type ctxKey struct{}

// With кладёт сообщение в контекст запроса.
func With(ctx context.Context, f *Flash) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

// FromContext — сообщение для отрисовки; nil, если показывать нечего.
func FromContext(ctx context.Context) *Flash {
	f, _ := ctx.Value(ctxKey{}).(*Flash)
	return f
}
