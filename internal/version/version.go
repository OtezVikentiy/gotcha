// Package version — единая точка сведений о версии бинаря gotcha.
// base — канон версии в репозитории: его двигает `make release`, и он совпадает
// с git-тегом vX.Y.Z. Переменные version/commit/date перезаписываются при
// сборке через -ldflags -X (см. Makefile и Dockerfile).
//
// Сборка через `docker compose build` (штатный путь обновления в доках) не
// прокидывает git-версию — Dockerfile передаёт ARG VERSION=dev. Такой сентинел
// («dev»/пусто/base+"-dev") резолвится в base, поэтому релизная сборка честно
// показывает версию релиза, а не «dev». Точную git-версию (с суффиксом -N-gHASH)
// даёт сборка через `make` (up-rebuild/build), которая её вычисляет.
package version

import (
	"runtime"
	"strings"
)

const base = "0.29.0"

var (
	version = "" // git describe --tags --always --dirty (через ldflags)
	commit  = "" // git rev-parse --short HEAD
	date    = "" // дата сборки, RFC3339 UTC
)

// resolved — итоговая строка версии: git-описание из ldflags, если оно осмысленно;
// иначе канон base (сборки без git-версии показывают версию релиза, а не «dev»).
func resolved() string {
	switch version {
	case "", "dev", base + "-dev":
		return base
	default:
		return version
	}
}

// Version — сырая строка версии: "v0.2.0" | "v0.2.0-5-gabcdef-dirty" | "0.2.0".
func Version() string { return resolved() }

// Stamped — были ли в сборку вшиты git-метаданные. Сентинелы ""/dev/base-dev
// резолвятся в base (см. resolved) — это осознанно: релизный образ должен
// называть версию релиза. Но происхождение значения — другой вопрос: сборка
// мимо make (docker compose build руками) выдаёт ровно ту же строку, и
// «развёрнуто именно то, что вы думаете» перестаёт быть проверяемым.
func Stamped() bool {
	switch version {
	case "", "dev", base + "-dev":
		return false
	}
	return true
}

// String — человекочитаемо: "v0.2.0 (abcdef, 2026-07-22)" либо честное
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

// Info — машиночитаемая форма для JSON /version и страницы About.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	Stamped bool   `json:"stamped"`
}

// Get — снимок сведений о версии.
func Get() Info {
	return Info{Version: resolved(), Commit: commit, Date: date, Go: runtime.Version(), Stamped: Stamped()}
}
