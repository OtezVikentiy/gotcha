// Package profile — приём, хранение и flamegraph-визуализация профилей (этап 7).
package profile

import (
	"strconv"
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

// FrameKey сериализует кадр в одну строку для колонки stack Array(String):
// "func (file:line)" либо "func", если файла нет.
func FrameKey(f Frame) string {
	if f.File == "" {
		return f.Function
	}
	return f.Function + " (" + f.File + ":" + strconv.FormatInt(int64(f.Line), 10) + ")"
}
