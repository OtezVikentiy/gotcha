package profile_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

func TestFrameKeyNoCollision(t *testing.T) {
	withParensInName := profile.FrameKey(profile.Frame{Function: "a (b:1)"})
	realFileLine := profile.FrameKey(profile.Frame{Function: "a", File: "b", Line: 1})
	if withParensInName == realFileLine {
		t.Fatalf("collision: %q == %q", withParensInName, realFileLine)
	}

	jsAnon := profile.FrameKey(profile.Frame{Function: "<anonymous> (app.js:10)"})
	jsFrame := profile.FrameKey(profile.Frame{Function: "<anonymous>", File: "app.js", Line: 10})
	if jsAnon == jsFrame {
		t.Fatalf("collision: %q == %q", jsAnon, jsFrame)
	}

	frames := []profile.Frame{
		{Function: "a (b:1)"},
		{Function: "a", File: "b", Line: 1},
		{Function: "a", File: "b", Line: 2},
		{Function: "a", File: "c", Line: 1},
		{Function: "a:b", File: "c", Line: 1},
		{Function: "a", File: "b:c", Line: 1},
		{Function: "a (b", File: "c", Line: 1},
		{Function: "a", File: "b) (c", Line: 1},
		{Function: "<anonymous> (app.js:10)"},
		{Function: "<anonymous>", File: "app.js", Line: 10},
	}
	seen := map[string]profile.Frame{}
	for _, f := range frames {
		k := profile.FrameKey(f)
		if prev, dup := seen[k]; dup {
			t.Fatalf("key %q produced by both %+v and %+v", k, prev, f)
		}
		seen[k] = f
	}
}

func TestFrameKeyReadableCommonCase(t *testing.T) {
	if got := profile.FrameKey(profile.Frame{Function: "runtime.mallocgc"}); got != "runtime.mallocgc" {
		t.Fatalf("FrameKey no-file = %q, want unchanged", got)
	}
	if got := profile.FrameKey(profile.Frame{Function: "main.foo", File: "main.go", Line: 42}); got != "main.foo (main.go:42)" {
		t.Fatalf("FrameKey = %q, want main.foo (main.go:42)", got)
	}
}
