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

// Экранирует разделители ключа кадра "func (file:line)" и разделитель стека
// stackSep (writer.go): без последнего U+001F внутри имени функции/файла
// склеил бы два разных стека в один ключ агрегации.
var frameFieldEscaper = strings.NewReplacer(`\`, `\\`, `(`, `\(`, `:`, `\:`, stackSep, `\`+stackSep)

func FrameKey(f Frame) string {
	if f.File == "" {
		return frameFieldEscaper.Replace(f.Function)
	}
	return frameFieldEscaper.Replace(f.Function) + " (" + frameFieldEscaper.Replace(f.File) + ":" + strconv.FormatInt(int64(f.Line), 10) + ")"
}
