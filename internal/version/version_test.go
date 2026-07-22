package version

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestVersionDefaultHasDevSuffix(t *testing.T) {
	if !strings.HasSuffix(Version(), "-dev") {
		t.Fatalf("ждали суффикс -dev в дефолтной сборке, получили %q", Version())
	}
	if !strings.HasPrefix(Version(), base) {
		t.Fatalf("ждали базу %q в начале %q", base, Version())
	}
}

func TestStringWithoutBuildMetadata(t *testing.T) {
	// commit/date пусты в дефолте — скобок быть не должно.
	if strings.Contains(String(), "(") {
		t.Fatalf("не ждали build-метаданных в %q", String())
	}
}

func TestStringWithBuildMetadata(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
	version, commit, date = "v1.2.3", "abcdef", "2026-07-22"
	if got, want := String(), "v1.2.3 (abcdef, 2026-07-22)"; got != want {
		t.Fatalf("String() = %q, ждали %q", got, want)
	}
}

func TestStringWithCommitOnly(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
	version, commit, date = "v1.2.3", "abcdef", ""
	if got, want := String(), "v1.2.3 (abcdef)"; got != want {
		t.Fatalf("String() = %q, ждали %q", got, want)
	}
}

func TestStringWithDateOnly(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
	version, commit, date = "v1.2.3", "", "2026-07-22"
	if got, want := String(), "v1.2.3 (2026-07-22)"; got != want {
		t.Fatalf("String() = %q, ждали %q", got, want)
	}
}

func TestGetShape(t *testing.T) {
	info := Get()
	if info.Version != Version() {
		t.Fatalf("Version рассинхрон: %q != %q", info.Version, Version())
	}
	if info.Go != runtime.Version() {
		t.Fatalf("Go = %q, ждали %q", info.Go, runtime.Version())
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version"`, `"commit"`, `"date"`, `"go"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("в JSON нет ключа %s: %s", key, b)
		}
	}
}
