package i18n

import (
	"context"
	"strings"
)

type Locale struct {
	Code string
}

type ctxKey struct{}

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

func T(ctx context.Context, key string) string {
	return lookup(FromContext(ctx).Code, key)
}

func Tf(ctx context.Context, key string, kv ...string) string {
	s := T(ctx, key)
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, "{"+kv[i]+"}", kv[i+1])
	}
	return s
}
