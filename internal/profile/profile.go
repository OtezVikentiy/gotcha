// Package profile — приём, хранение и flamegraph-визуализация профилей (этап 7).
package profile

import (
	"strconv"
	"strings"
	"time"
)

// Frame — один кадр стека (уже символизированный: имя функции, файл, строка).
type Frame struct {
	Function string
	File     string
	Line     int32
}

// Sample — уникальный стек с весом (число сэмплов / value выбранного sample-type).
type Sample struct {
	Stack []Frame // корень→лист
	Value uint64
}

// Profile — нормализованный профиль (общая модель для Sentry и pprof форматов).
type Profile struct {
	Service, Environment, Transaction, Platform, Type string // Type: 'cpu'|'wall'|'alloc'|...
	// TraceID — привязка к трейсу (этап 8, profiling-in-context). Пусто —
	// профиль без привязки (напр. непрерывный pprof).
	TraceID   string
	Timestamp time.Time
	Samples   []Sample
}

// frameFieldEscaper экранирует символы, из которых собран разделитель ключа
// кадра, чтобы сериализация была инъективной (разные (Function,File,Line) → разные
// ключи). Без экранирования имя вроде "a (b:1)" без файла давало тот же ключ, что
// {Function:"a", File:"b", Line:1}, и агрегатор writer.go схлопывал несвязанные
// стеки. Экранируем: '\\' (делает схему обратимой), '(' (граница func/file),
// ':' (граница file/line). Обратный слэш идёт первым — strings.Replacer не
// перечитывает вставленное, так что двойного экранирования нет.
var frameFieldEscaper = strings.NewReplacer(`\`, `\\`, `(`, `\(`, `:`, `\:`)

// FrameKey сериализует кадр в одну строку для колонки stack Array(String):
// "func (file:line)" либо "func", если файла нет. Поля экранируются, поэтому
// разные кадры никогда не дают одинаковый ключ (см. frameFieldEscaper).
func FrameKey(f Frame) string {
	if f.File == "" {
		return frameFieldEscaper.Replace(f.Function)
	}
	return frameFieldEscaper.Replace(f.Function) + " (" + frameFieldEscaper.Replace(f.File) + ":" + strconv.FormatInt(int64(f.Line), 10) + ")"
}
