package version

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// Сборки без git-версии (version="" при go build, "dev" при docker compose
// build, base+"-dev" — старый дефолт) резолвятся в чистый base, а не «dev»:
// именно из-за «dev» релизная сборка показывала неверную версию.
func TestVersionDefaultResolvesToBase(t *testing.T) {
	if got := Version(); got != base {
		t.Fatalf("дефолтная версия = %q, ждали base %q", got, base)
	}
	oldV := version
	t.Cleanup(func() { version = oldV })
	for _, sentinel := range []string{"", "dev", base + "-dev"} {
		version = sentinel
		if got := Version(); got != base {
			t.Fatalf("version=%q → Version()=%q, ждали base %q", sentinel, got, base)
		}
	}
}

func TestStringWithoutBuildMetadata(t *testing.T) {
	// commit/date пусты в дефолте — вместо них честная пометка о том, что
	// git-метаданные в сборку не вшиты (находка №102: сборка мимо make
	// выдавала неотличимую от релиза строку).
	if got, want := String(), base+" (no build metadata)"; got != want {
		t.Fatalf("String() = %q, ждали %q", got, want)
	}
}

// TestStamped — Stamped() отличает вшитую git-версию от сентинелов сборки без
// метаданных; Get().Stamped согласован, String() помечает несштампованную.
func TestStamped(t *testing.T) {
	oldV := version
	t.Cleanup(func() { version = oldV })
	for _, sentinel := range []string{"", "dev", base + "-dev"} {
		version = sentinel
		if Stamped() {
			t.Errorf("version=%q: Stamped()=true, ждали false", sentinel)
		}
		if !strings.Contains(String(), "no build metadata") {
			t.Errorf("version=%q: String()=%q без пометки", sentinel, String())
		}
		if Get().Stamped {
			t.Errorf("version=%q: Get().Stamped=true, ждали false", sentinel)
		}
	}
	version = "v0.4.1-3-gabcdef"
	if !Stamped() {
		t.Error("git-версия: Stamped()=false, ждали true")
	}
	if strings.Contains(String(), "no build metadata") {
		t.Errorf("git-версия: String()=%q с пометкой", String())
	}
	if got := Version(); got != "v0.4.1-3-gabcdef" {
		t.Errorf("Version()=%q, ждали v0.4.1-3-gabcdef", got)
	}
	if !Get().Stamped {
		t.Error("git-версия: Get().Stamped=false, ждали true")
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
