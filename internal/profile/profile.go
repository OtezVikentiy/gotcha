package profile

import (
	"strconv"
	"strings"
	"time"
)

type Frame struct {
	Function string
	File     string
	Line     int32
}

type Sample struct {
	Stack []Frame
	Value uint64
}

type Profile struct {
	Service, Environment, Transaction, Platform, Type string
	Unit                                              string
	TraceID                                           string
	Timestamp                                         time.Time
	Samples                                           []Sample
	// Truncated: часть сэмплов/кадров срезана капом приёма, а не самим клиентом.
	// Только для наблюдаемости на приёме — не персистится и не отдаётся клиенту.
	Truncated bool
}

// Экранирует ровно разделители ключа кадра "func (file:line)": без этого
// разные (Function,File,Line) могут дать одинаковый ключ и слиться в writer.go.
var frameFieldEscaper = strings.NewReplacer(`\`, `\\`, `(`, `\(`, `:`, `\:`)

func FrameKey(f Frame) string {
	if f.File == "" {
		return frameFieldEscaper.Replace(f.Function)
	}
	return frameFieldEscaper.Replace(f.Function) + " (" + frameFieldEscaper.Replace(f.File) + ":" + strconv.FormatInt(int64(f.Line), 10) + ")"
}
