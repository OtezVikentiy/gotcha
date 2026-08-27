package humanize_test

import (
	"context"
	"strings"
	"testing"
	"time"

	// Часовые пояса вкомпилированы в тест: пакет читает time.LoadLocation, но
	// не подключает internal/testenv, где обычно живёт этот импорт — так что
	// подключаем сами, иначе в slim-контейнере без /usr/share/zoneinfo тест
	// падает не по вине проверяемого кода.
	_ "time/tzdata"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// testCtx — контекст с локалью по умолчанию (ru), как кладёт web-миддлвара в
// проде. Приём взят из internal/web/templates (см. helpers_test.go: ruCtx).
func testCtx(t *testing.T) context.Context {
	t.Helper()
	return i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
}

// ctxWithLocale — контекст с указанной локалью, для тестов, сравнивающих
// перевод между языками.
func ctxWithLocale(t *testing.T, code string) context.Context {
	t.Helper()
	return i18n.WithLocale(context.Background(), i18n.Locale{Code: code})
}

// TestMetricValueDurationIsMilliseconds: после сведения единиц duration
// приходит в миллисекундах. Раньше та же величина приезжала микросекундной, и
// 640 мс показывались как «640.0s» — завышение ровно в тысячу раз.
func TestMetricValueDurationIsMilliseconds(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.MetricValue(ctx, "duration", 640); !strings.Contains(got, "640") {
		t.Fatalf("MetricValue(duration, 640) = %q, ждали 640 миллисекунд", got)
	}
	if got := humanize.MetricValue(ctx, "duration", 640); strings.Contains(got, "640.0s") {
		t.Fatalf("MetricValue(duration, 640) = %q — значение трактовано как микросекунды", got)
	}
}

// TestMetricValueCLSIsDimensionless: CLS — отношение, единицы у него нет.
func TestMetricValueCLSIsDimensionless(t *testing.T) {
	ctx := testCtx(t)
	got := humanize.MetricValue(ctx, "cls", 0.25)
	if strings.ContainsAny(got, "sm") {
		t.Fatalf("MetricValue(cls, 0.25) = %q — безразмерной величине приписана единица", got)
	}
}

// TestDurationLocalises: длительность — единственная величина пакета, которая
// переводится; формат времени и дат числовой в обеих локалях намеренно.
func TestDurationLocalises(t *testing.T) {
	ru, en := ctxWithLocale(t, "ru"), ctxWithLocale(t, "en")
	if humanize.Duration(ru, 2*time.Hour) == humanize.Duration(en, 2*time.Hour) {
		t.Fatal("длительность одинакова в обеих локалях — каталог не подключён")
	}
}

// TestMetricValueDurationSecondsThreshold — на границе секунды и выше duration
// переходит на секунды с одним знаком после запятой (formatMetric-формула,
// верная теперь, когда вход уже в мс).
func TestMetricValueDurationSecondsThreshold(t *testing.T) {
	ctx := testCtx(t)
	cases := map[float64]string{
		999:  "999ms",
		1000: "1.0s",
		1500: "1.5s",
	}
	for in, want := range cases {
		if got := humanize.MetricValue(ctx, "duration", in); got != want {
			t.Errorf("MetricValue(duration, %v) = %q, want %q", in, got, want)
		}
	}
}

// TestMetricValueDurationNegativeClampedToZero — отрицательное значение (сбой
// сбора метрик) не должно превращаться в отрицательную длительность на
// экране. После клэмпа v=0 попадает в под-миллисекундную ветку (0 < 1мс), как
// и любой настоящий ноль — см. TestMetricValueDurationZero.
func TestMetricValueDurationNegativeClampedToZero(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.MetricValue(ctx, "duration", -50); got != "0µs" {
		t.Errorf("MetricValue(duration, -50) = %q, want %q", got, "0µs")
	}
}

// TestMetricValueDurationSubMillisecond — p95 эндпойнта в 900 микросекунд
// приезжает как MetricValue(ctx, "duration", 0.9) (вход уже в мс, задача 1).
// Округление до целых миллисекунд дало бы «0ms» — неправду о работающем
// быстро эндпойнте; вместо этого ветка показывает микросекунды.
func TestMetricValueDurationSubMillisecond(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.MetricValue(ctx, "duration", 0.9); got != "900µs" {
		t.Errorf("MetricValue(duration, 0.9) = %q, want %q", got, "900µs")
	}
}

// TestMetricValueDurationOneMillisecondBoundary — ровно на границе 1мс ветка
// переходит на целые миллисекунды, а не на микросекунды (иначе получили бы
// «1000µs» вместо «1ms» — тот же смысл, но не та форма, которую ждёт брифом
// заданный формат «целым числом с суффиксом до секунды»).
func TestMetricValueDurationOneMillisecondBoundary(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.MetricValue(ctx, "duration", 1); got != "1ms" {
		t.Errorf("MetricValue(duration, 1) = %q, want %q", got, "1ms")
	}
}

// TestMetricValueDurationZero — настоящий (не клэмпнутый) ноль: никакая
// информация не теряется ни в одной из единиц, но ветка сама по себе не
// должна давать деление на ноль или отрицательный ноль на экране.
func TestMetricValueDurationZero(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.MetricValue(ctx, "duration", 0); got != "0µs" {
		t.Errorf("MetricValue(duration, 0) = %q, want %q", got, "0µs")
	}
}

// TestMetricValueVitalsUseTwoDecimalSeconds — веб-виталы (и любая неизвестная
// метрика) форматируются как formatVitalMS в остальном интерфейсе: секунды с
// двумя знаками после запятой, в отличие от duration с одним.
func TestMetricValueVitalsUseTwoDecimalSeconds(t *testing.T) {
	ctx := testCtx(t)
	cases := map[string]struct {
		metric string
		v      float64
		want   string
	}{
		"lcp под секундой":    {"lcp", 800, "800ms"},
		"lcp над секундой":    {"lcp", 2500, "2.50s"},
		"неизвестная метрика": {"unknown-metric", 1200, "1.20s"},
		"нулевое значение":    {"inp", 0, "0ms"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := humanize.MetricValue(ctx, c.metric, c.v); got != c.want {
				t.Errorf("MetricValue(%q, %v) = %q, want %q", c.metric, c.v, got, c.want)
			}
		})
	}
}

// TestAgo — все пороги относительного времени плюс отрицательный перекос
// часов, приравниваемый к нулю.
func TestAgo(t *testing.T) {
	ctx := testCtx(t)
	cases := map[string]time.Time{
		"только что":              time.Now(),
		"будущее (перекос часов)": time.Now().Add(5 * time.Second),
		"секунды":                 time.Now().Add(-30 * time.Second),
		"минуты":                  time.Now().Add(-5 * time.Minute),
		"часы":                    time.Now().Add(-3 * time.Hour),
		"дни":                     time.Now().Add(-3 * 24 * time.Hour),
	}
	for name, tm := range cases {
		t.Run(name, func(t *testing.T) {
			got := humanize.Ago(ctx, tm)
			if got == "" {
				t.Errorf("Ago(%v) вернул пустую строку", tm)
			}
		})
	}
	if got := humanize.Ago(ctx, time.Now().Add(5*time.Second)); got != i18n.T(ctx, "time.just_now") {
		t.Errorf("Ago для времени в будущем = %q, ждали %q (перекос часов приравнен к нулю)", got, i18n.T(ctx, "time.just_now"))
	}
}

// TestTime — базовое форматирование, zero-time, nil-пояс и что подпись пояса
// присутствует (см. докблок про «время без указания часового пояса»).
func TestTime(t *testing.T) {
	ctx := testCtx(t)
	if got := humanize.Time(ctx, time.Time{}, time.UTC); got != "" {
		t.Errorf("Time(zero) = %q, want empty", got)
	}

	moment := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	if got := humanize.Time(ctx, moment, nil); !strings.Contains(got, "UTC") {
		t.Errorf("Time с nil-поясом = %q, ждали фолбэк на UTC", got)
	}

	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("нет базы часовых поясов: %v", err)
	}
	got := humanize.Time(ctx, moment, loc)
	if !strings.Contains(got, "2026-07-31") {
		t.Errorf("Time(Europe/Moscow) = %q, дата потерялась", got)
	}
	zone, _ := moment.In(loc).Zone()
	if !strings.Contains(got, zone) {
		t.Errorf("Time(Europe/Moscow) = %q, ждали подпись пояса %q", got, zone)
	}
}

// TestTimeSameAcrossLocales — формат времени числовой и одинаковый в обеих
// локалях намеренно (докблок Time); проверяем, что это действительно так.
func TestTimeSameAcrossLocales(t *testing.T) {
	moment := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)
	ru, en := ctxWithLocale(t, "ru"), ctxWithLocale(t, "en")
	if humanize.Time(ru, moment, time.UTC) != humanize.Time(en, moment, time.UTC) {
		t.Fatal("Time зависит от локали — по докблоку не должен")
	}
}

// TestDurationAllThresholds — все пороги (дни/часы/минуты/секунды/«меньше
// минуты») плюс отрицательная длительность, приводимая к модулю.
func TestDurationAllThresholds(t *testing.T) {
	ctx := testCtx(t)
	cases := map[string]time.Duration{
		"дни":            3 * 24 * time.Hour,
		"часы":           5 * time.Hour,
		"минуты":         10 * time.Minute,
		"секунды":        30 * time.Second,
		"меньше секунды": 500 * time.Millisecond,
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if got := humanize.Duration(ctx, d); got == "" {
				t.Errorf("Duration(%v) вернул пустую строку", d)
			}
		})
	}
	if humanize.Duration(ctx, -5*time.Hour) != humanize.Duration(ctx, 5*time.Hour) {
		t.Error("Duration для отрицательной длительности не приравнена к модулю")
	}
}

// TestLocationOrUTC — пустое имя, некорректное имя и валидный пояс.
func TestLocationOrUTC(t *testing.T) {
	if got := humanize.LocationOrUTC(""); got != time.UTC {
		t.Errorf("LocationOrUTC(\"\") = %v, want UTC", got)
	}
	if got := humanize.LocationOrUTC("   "); got != time.UTC {
		t.Errorf("LocationOrUTC(пробелы) = %v, want UTC", got)
	}
	if got := humanize.LocationOrUTC("Not/ARealZone"); got != time.UTC {
		t.Errorf("LocationOrUTC(неизвестный пояс) = %v, want UTC (fallback)", got)
	}
	if got := humanize.LocationOrUTC("Europe/Moscow"); got == time.UTC || got.String() != "Europe/Moscow" {
		t.Errorf("LocationOrUTC(Europe/Moscow) = %v, want сам пояс", got)
	}
}

// TestCompactNumber — компактная запись чисел метрик: суффиксы k/M/G/T,
// три значащие цифры, никакой научной нотации для крупных значений
// (QA-находка «avg > 8e+08» на странице правил метрик).
func TestCompactNumber(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "0"},
		{17, "17"},
		{10.5, "10.5"},
		{0.25, "0.25"},
		{999, "999"},
		{1500, "1.5k"},
		{8e8, "800M"},
		{9.1e8, "910M"},
		{1.25e9, "1.25G"},
		{5e12, "5T"},
		{-1500, "-1.5k"},
		{123.456, "123"},
		{0.000005, "5e-06"},
	}
	for _, c := range cases {
		if got := humanize.CompactNumber(c.v); got != c.want {
			t.Errorf("CompactNumber(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0B"},
		{-1, "0B"},
		{-1 << 20, "0B"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024*1024 - 1, "1024.0KB"},
		{1024 * 1024, "1.0MB"},
		{10 * 1024 * 1024, "10.0MB"},
		{1024 * 1024 * 1024, "1.00GB"},
		{5 * 1024 * 1024 * 1024, "5.00GB"},
	}
	for _, c := range cases {
		if got := humanize.Bytes(c.b); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}
