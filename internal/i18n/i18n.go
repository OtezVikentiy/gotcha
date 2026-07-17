// Package i18n — локализация SSR-интерфейса Gotcha. Локаль живёт в
// context.Context (кладёт web-миддлвара), шаблоны зовут T/Tf/Tn.
package i18n

import (
	"context"
	"strings"
)

// Locale — выбранный язык. Code — базовый код "ru"/"en".
type Locale struct {
	Code string
}

type ctxKey struct{}

// Default — язык по умолчанию и fallback всей цепочки разрешения.
var Default = Locale{Code: "ru"}

func WithLocale(ctx context.Context, l Locale) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

func FromContext(ctx context.Context) Locale {
	if l, ok := ctx.Value(ctxKey{}).(Locale); ok && l.Code != "" {
		return l
	}
	return Default
}

// T — перевод ключа для локали из ctx. Fallback: дефолтная локаль → сам ключ.
func T(ctx context.Context, key string) string {
	return lookup(FromContext(ctx).Code, key)
}

// Tf — как T, но подставляет {name} из пар kv (k1,v1,k2,v2,...).
func Tf(ctx context.Context, key string, kv ...string) string {
	s := T(ctx, key)
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, "{"+kv[i]+"}", kv[i+1])
	}
	return s
}
