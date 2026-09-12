package theme

import "context"

// Code ∈ "dark"|"light"|"system".
type Theme struct{ Code string }

type ctxKey struct{}

var Default = Theme{Code: "system"}

var known = map[string]bool{"dark": true, "light": true, "system": true}

func WithTheme(ctx context.Context, t Theme) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

func FromContext(ctx context.Context) Theme {
	if t, ok := ctx.Value(ctxKey{}).(Theme); ok && t.Code != "" {
		return t
	}
	return Default
}

func Parse(code string) (Theme, bool) {
	if known[code] {
		return Theme{Code: code}, true
	}
	return Theme{}, false
}
