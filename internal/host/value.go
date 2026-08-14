package host

import (
	"context"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
)

// ValueLabel форматирует значение инцидента с юнитами ВИДА порога:
// диск/память — доля (0..1] в проценты с одним знаком после запятой (те же
// правила, что humanize.MetricValue — 1 знак у процентных величин, см.
// profileregressions_templ.go), нагрузка — множитель ядра, тишина —
// длительность через humanize.Duration (сторож
// TestNoRawTimeFormattingOutsideHumanize требует форматировать время только
// там).
//
// Экспортирована, потому что потребителей у неё два, и оба обязаны
// показывать одно и то же число одинаково (UX-аудит A1, P1-3): текст
// уведомления (hostBody, notify.go) и карточка хоста
// (hostOpenIncidentRow/hostIncidentRow, hostdetail.templ). Карточка раньше
// печатала сырое CurrentValue через fmtFloat, и это давало прямое
// расхождение внутри ОДНОГО экрана: в списке хостов строкой выше диск
// показан как «93%», а в блоке инцидентов того же хоста — «0.93»; тишина же
// печаталась в секундах («3.6K»), то есть единицу измерения читатель должен
// был угадать. Дублировать таблицу «вид → формат» в шаблонах нельзя — она
// ровно та, по которой сверстаны пороги и тексты уведомлений, и разъехаться
// им нечем, кроме забывчивости.
//
// kind — значение из Kinds; незнакомый вид (сюда попасть не должен) даёт
// голое число с двумя знаками, а не панику или пустую строку.
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
