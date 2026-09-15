package version

import (
	"runtime"
	"strings"
)

const base = "1.5.0"

var (
	version = "" // git describe --tags --always --dirty (через ldflags)
	commit  = "" // git rev-parse --short HEAD
	date    = "" // дата сборки, RFC3339 UTC
)

// сборки без git-версии показывают версию релиза (base), не «dev».
func resolved() string {
	switch version {
	case "", "dev", base + "-dev":
		return base
	default:
		return version
	}
}

// сырая строка версии: "v0.2.0" | "v0.2.0-5-gabcdef-dirty" | "0.2.0".
func Version() string { return resolved() }

// сентинелы ""/dev/base-dev резолвятся в false; но сборка мимо make
// (docker compose build вручную) даёт ту же строку — Stamped() это не различит.
func Stamped() bool {
	switch version {
	case "", "dev", base + "-dev":
		return false
	}
	return true
}

// человекочитаемо: "v0.2.0 (abcdef, 2026-07-22)" либо честное
// "0.2.0 (no build metadata)" для сборки без вшитой git-версии.
func String() string {
	var b strings.Builder
	b.WriteString(resolved())
	switch {
	case commit != "" && date != "":
		b.WriteString(" (" + commit + ", " + date + ")")
	case commit != "":
		b.WriteString(" (" + commit + ")")
	case date != "":
		b.WriteString(" (" + date + ")")
	}
	if !Stamped() {
		b.WriteString(" (no build metadata)")
	}
	return b.String()
}

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	Stamped bool   `json:"stamped"`
}

func Get() Info {
	return Info{Version: resolved(), Commit: commit, Date: date, Go: runtime.Version(), Stamped: Stamped()}
}
