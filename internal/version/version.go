// Package version — единая точка сведений о версии бинаря gotcha.
// base — канон версии в репозитории: его двигает `make release`, и он совпадает
// с git-тегом vX.Y.Z. Переменные version/commit/date перезаписываются при
// сборке через -ldflags -X (см. Makefile и Dockerfile); без ldflags бинарь
// честно докладывает "<base>-dev".
package version

import (
	"runtime"
	"strings"
)

const base = "0.1.0"

var (
	version = base + "-dev" // git describe --tags --always --dirty
	commit  = ""            // git rev-parse --short HEAD
	date    = ""            // дата сборки, RFC3339 UTC
)

// Version — сырая строка версии: "v0.1.0" | "v0.1.0-5-gabcdef-dirty" | "0.1.0-dev".
func Version() string { return version }

// String — человекочитаемо: "0.1.0-dev" либо "v0.1.0 (abcdef, 2026-07-22)".
func String() string {
	var b strings.Builder
	b.WriteString(version)
	switch {
	case commit != "" && date != "":
		b.WriteString(" (" + commit + ", " + date + ")")
	case commit != "":
		b.WriteString(" (" + commit + ")")
	case date != "":
		b.WriteString(" (" + date + ")")
	}
	return b.String()
}

// Info — машиночитаемая форма для JSON /version и страницы About.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Get — снимок сведений о версии.
func Get() Info {
	return Info{Version: version, Commit: commit, Date: date, Go: runtime.Version()}
}
