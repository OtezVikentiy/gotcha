package flashctx

import "context"

type Flash struct {
	Kind string
	// Хранится КЛЮЧ, а не текст: cookie ставит клиент, и произвольный текст оттуда
	// был бы площадкой для фишинга на нашей же странице.
	Key string
	// 0 → обычный перевод, не форма i18n.Tn.
	N int
	// Осмысленно только вместе с Pair.
	M int
	// Ставится parseFlash по ключу (белый список flashPairKeys) — через cookie не
	// передаётся и клиентом не подделывается.
	Pair bool
}

type ctxKey struct{}

func With(ctx context.Context, f *Flash) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

func FromContext(ctx context.Context) *Flash {
	f, _ := ctx.Value(ctxKey{}).(*Flash)
	return f
}
