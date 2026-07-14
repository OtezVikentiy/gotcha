// Package fingerprint превращает событие в отпечаток группы.
// Приоритет: кастомный fingerprint из SDK → нормализованный stacktrace →
// exception type + нормализованное сообщение → нормализованное сообщение.
package fingerprint

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

type Frame struct {
	Module   string
	Function string
	InApp    bool
}

type Exception struct {
	Type   string
	Value  string
	Frames []Frame
}

type Input struct {
	Custom     []string // fingerprint из события; может содержать "{{ default }}"
	Exceptions []Exception
	Message    string
}

// Compute возвращает hex sha1 отпечатка группы.
func Compute(in Input) string {
	base := defaultComponent(in)
	if len(in.Custom) > 0 {
		parts := make([]string, len(in.Custom))
		for i, p := range in.Custom {
			if strings.TrimSpace(p) == "{{ default }}" {
				parts[i] = base
			} else {
				parts[i] = p
			}
		}
		return hash("custom\x00" + strings.Join(parts, "\x00"))
	}
	return hash(base)
}

func defaultComponent(in Input) string {
	if s := stackComponent(in.Exceptions); s != "" {
		return "stack\x00" + s
	}
	if len(in.Exceptions) > 0 {
		// Последний exception в цепочке — фактически брошенный.
		ex := in.Exceptions[len(in.Exceptions)-1]
		if ex.Type != "" {
			return "exc\x00" + ex.Type + "\x00" + NormalizeMessage(ex.Value)
		}
	}
	return "msg\x00" + NormalizeMessage(in.Message)
}

// stackComponent: для каждого exception — тип и in-app фреймы
// (module|function, без номеров строк); если in-app нет — все фреймы.
// Пусто, если ни в одном exception нет ни одного пригодного фрейма.
func stackComponent(excs []Exception) string {
	var b strings.Builder
	hasFrames := false
	for _, ex := range excs {
		frames := ex.Frames
		var inApp []Frame
		for _, f := range frames {
			if f.InApp {
				inApp = append(inApp, f)
			}
		}
		if len(inApp) > 0 {
			frames = inApp
		}
		wrote := false
		for _, f := range frames {
			if f.Module == "" && f.Function == "" {
				continue
			}
			b.WriteString(f.Module)
			b.WriteByte('|')
			b.WriteString(f.Function)
			b.WriteByte('\n')
			wrote = true
		}
		if wrote {
			b.WriteString(ex.Type)
			b.WriteByte('\n')
			hasFrames = true
		}
	}
	if !hasFrames {
		return ""
	}
	return b.String()
}

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
