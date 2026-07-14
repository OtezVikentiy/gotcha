package fingerprint

import "testing"

func frames(inApp bool, pairs ...string) []Frame {
	var fs []Frame
	for i := 0; i+1 < len(pairs); i += 2 {
		fs = append(fs, Frame{Module: pairs[i], Function: pairs[i+1], InApp: inApp})
	}
	return fs
}

func TestComputePriorities(t *testing.T) {
	stackIn := Input{Exceptions: []Exception{{
		Type: "ValueError", Value: "bad id 42",
		Frames: append(frames(false, "django.core", "handler"), frames(true, "app.views", "get_user")...),
	}}}

	// 1. Стек стабилен: другой message/value — тот же отпечаток.
	other := stackIn
	other.Exceptions = []Exception{{
		Type: "ValueError", Value: "bad id 777",
		Frames: stackIn.Exceptions[0].Frames,
	}}
	if Compute(stackIn) != Compute(other) {
		t.Error("same stack, different value: fingerprints must match")
	}

	// 2. Только in-app фреймы участвуют: добавление системного фрейма не меняет отпечаток.
	withExtraSystem := stackIn
	withExtraSystem.Exceptions = []Exception{{
		Type: "ValueError", Value: "bad id 42",
		Frames: append(frames(false, "urllib3", "request"), stackIn.Exceptions[0].Frames...),
	}}
	if Compute(stackIn) != Compute(withExtraSystem) {
		t.Error("extra system frame must not change fingerprint")
	}

	// 3. Другой in-app стек — другой отпечаток.
	otherStack := Input{Exceptions: []Exception{{
		Type: "ValueError", Value: "bad id 42",
		Frames: frames(true, "app.views", "delete_user"),
	}}}
	if Compute(stackIn) == Compute(otherStack) {
		t.Error("different in-app stack must change fingerprint")
	}

	// 4. Нет in-app — используются все фреймы.
	sysOnly := Input{Exceptions: []Exception{{
		Type: "OperationalError", Value: "x",
		Frames: frames(false, "psycopg2", "connect"),
	}}}
	sysOnly2 := Input{Exceptions: []Exception{{
		Type: "OperationalError", Value: "x",
		Frames: frames(false, "psycopg2", "execute"),
	}}}
	if Compute(sysOnly) == Compute(sysOnly2) {
		t.Error("system-only stacks with different frames must differ")
	}

	// 5. Нет стека: type + нормализованное value.
	noStack1 := Input{Exceptions: []Exception{{Type: "UserNotFound", Value: "user 123 not found"}}}
	noStack2 := Input{Exceptions: []Exception{{Type: "UserNotFound", Value: "user 456 not found"}}}
	if Compute(noStack1) != Compute(noStack2) {
		t.Error("dynamic numbers in value must be normalized")
	}
	noStack3 := Input{Exceptions: []Exception{{Type: "OrderNotFound", Value: "user 123 not found"}}}
	if Compute(noStack1) == Compute(noStack3) {
		t.Error("different exception type must change fingerprint")
	}

	// 6. Совсем без exception: message.
	msg1 := Input{Message: "timeout after 30s for request 0xdeadbeef"}
	msg2 := Input{Message: "timeout after 60s for request 0xcafebabe"}
	if Compute(msg1) != Compute(msg2) {
		t.Error("message events must group after normalization")
	}

	// 7. Кастомный fingerprint перекрывает всё; {{ default }} подставляется.
	custom1 := stackIn
	custom1.Custom = []string{"payments", "timeout"}
	custom2 := otherStack
	custom2.Custom = []string{"payments", "timeout"}
	if Compute(custom1) != Compute(custom2) {
		t.Error("custom fingerprint must override stack difference")
	}
	withDefault := stackIn
	withDefault.Custom = []string{"{{ default }}", "shard-1"}
	withDefault2 := otherStack
	withDefault2.Custom = []string{"{{ default }}", "shard-1"}
	if Compute(withDefault) == Compute(withDefault2) {
		t.Error("{{ default }} must expand to the default component")
	}

	// 8. Пустой вход детерминирован и не паникует.
	if Compute(Input{}) == "" || Compute(Input{}) != Compute(Input{}) {
		t.Error("empty input must produce stable non-empty fingerprint")
	}
}

func TestNormalizeMessage(t *testing.T) {
	cases := map[string]string{
		"user 123 not found":                    "user <num> not found",
		"id 550e8400-e29b-41d4-a716-4466554400aa": "id <uuid>",
		"token deadbeefcafe0123":                 "token <hex>",
		"addr 0xDEADBEEF":                        "addr <hex>",
		"plain text":                             "plain text",
	}
	for in, want := range cases {
		if got := NormalizeMessage(in); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
