package templates

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
)

var customRange = TimeRangeVM{Key: "custom", Custom: true, Start: "2026-07-01T00:00", End: "2026-07-10T00:00"}

func TestTimeRangeFieldsCustom(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := timeRangeFields(customRange).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{`value="custom" selected`, `value="2026-07-01T00:00"`, `value="2026-07-10T00:00"`} {
		if !strings.Contains(out, want) {
			t.Errorf("timeRangeFields(custom) missing %q\n%s", want, out)
		}
	}
}

func TestTimeRangeLabelCustom(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := timeRangeLabel(customRange).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	if out := sb.String(); !strings.Contains(out, "2026-07-01 00:00 UTC") || !strings.Contains(out, "2026-07-10 00:00 UTC") {
		t.Errorf("timeRangeLabel(custom) = %q", out)
	}

	sb.Reset()
	if err := timeRangeLabel(TimeRangeVM{Key: "24h"}).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	if out := sb.String(); strings.TrimSpace(out) == "" {
		t.Error("timeRangeLabel(preset) empty")
	}

	sb.Reset()
	if err := timeRangeLabel(TimeRangeVM{Custom: true, Start: "raw-x", End: "raw-y"}).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	if out := sb.String(); !strings.Contains(out, "raw-x") {
		t.Errorf("prettyBound fallback lost the raw value: %q", out)
	}
}

// «02.01.2026» в англоязычном интерфейсе неоднозначен (2 января или 1 февраля).
// формат числовой и одинаковый в обеих локалях — тот же приём, что у humanize.Time.
func TestRangeBoundIsUnambiguous(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	got := prettyBound(ctx, "2026-01-02T15:04")
	if strings.HasPrefix(got, "02.01") {
		t.Fatalf("граница = %q: русский формат в интерфейсе обеих локалей", got)
	}
	if !strings.HasPrefix(got, "2026-01-02") {
		t.Fatalf("граница = %q, ждали 2026-01-02 15:04", got)
	}
}

func TestTimeRangeApply(t *testing.T) {
	q := url.Values{}
	customRange.apply(q)
	if q.Get("period") != "custom" || q.Get("start") == "" || q.Get("end") == "" {
		t.Errorf("apply(custom) = %v", q)
	}

	q = url.Values{}
	TimeRangeVM{Key: "7d"}.apply(q)
	if q.Get("period") != "7d" || q.Has("start") || q.Has("end") {
		t.Errorf("apply(preset) = %v", q)
	}
}

func TestTimeRangeURLBuildersCarryCustom(t *testing.T) {
	builders := map[string]string{
		"perfListSortURL":         perfListSortURL(7, PerfFilter{Range: customRange}, "p95"),
		"wvListSortURL":           wvListSortURL(7, PerfFilter{Range: customRange}, "count"),
		"metricLabelFilterURL":    metricLabelFilterURL(MetricDetailVM{ProjectID: 7, Range: customRange, Agg: "avg"}, "host", "api-01"),
		"issueEventPath":          issueEventPath(5, "ev1", customRange),
		"monitorIncidentsPageURL": monitorIncidentsPageURL(3, 2, customRange),
	}
	for name, got := range builders {
		if !strings.Contains(got, "period=custom") || !strings.Contains(got, "start=") || !strings.Contains(got, "end=") {
			t.Errorf("%s did not carry custom range: %s", name, got)
		}
	}
}

func TestBackTargetFromReferer(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = nav.WithShell(ctx, nav.Shell{Back: "/projects/7/incidents?incpage=2"})

	href, label := backTarget(ctx, "/projects/7/monitors", "Мониторы")
	if string(href) != "/projects/7/incidents?incpage=2" {
		t.Errorf("href = %q, want the referer path", href)
	}
	if label == "Мониторы" || label == "" {
		t.Errorf("label = %q, want the section-specific label", label)
	}

	ctx2 := nav.WithShell(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), nav.Shell{Back: "/whatever"})
	_, generic := backTarget(ctx2, "/x", "Default")
	if generic == "Default" || generic == "" {
		t.Errorf("unknown referer path label = %q, want generic nav.back", generic)
	}

	href3, label3 := backTarget(ctx, "", "")
	_ = href3
	if string(href3) == "" && label3 == "" {
		// эта ветка недостижима: ctx содержит Back; проверяем отдельным пустым ctx
	}
	empty := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	hrefD, labelD := backTarget(empty, "/default", "DefLabel")
	if string(hrefD) != "/default" || labelD != "DefLabel" {
		t.Errorf("no-Back fallback = (%q,%q), want default", hrefD, labelD)
	}
}
