package host

import (
	"context"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
)

// Общий формат для уведомлений и карточки: проценты как 93, не 0.93; секунды без юнита.
// Незнакомый kind — голое число с двумя знаками, не паника.
func ValueLabel(ctx context.Context, kind string, v float64) string {
	switch kind {
	case "disk", "memory":
		return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
	case "load":
		return strconv.FormatFloat(v, 'f', 2, 64) + "×"
	case "silent":
		return humanize.Duration(ctx, time.Duration(v*float64(time.Second)))
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}
