package templates

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestMonitorStatusTextKey — все статусы монитора имеют свой i18n-ключ,
// неизвестный падает в «unknown».
func TestMonitorStatusTextKey(t *testing.T) {
	cases := map[string]string{
		"up": "uptime.status.up", "down": "uptime.status.down",
		"paused": "uptime.status.paused", "maintenance": "uptime.status.maintenance",
		"weird": "uptime.status.unknown",
	}
	for in, want := range cases {
		if got := monitorStatusTextKey(in); got != want {
			t.Errorf("monitorStatusTextKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusMonitorTextLocalized — публичный статус монитора локализуется для
// всех значений и не пуст.
func TestStatusMonitorTextLocalized(t *testing.T) {
	ctx := ruCtx()
	for _, s := range []string{"up", "down", "paused", "maintenance", "weird"} {
		if got := statusMonitorText(ctx, s); got == "" {
			t.Errorf("statusMonitorText(%q) пусто", s)
		}
	}
}

// TestOverallStatusTextLocalized — сводный статус локализуется для
// major/partial/ok.
func TestOverallStatusTextLocalized(t *testing.T) {
	ctx := ruCtx()
	a, b, c := overallStatusText(ctx, "major"), overallStatusText(ctx, "partial"), overallStatusText(ctx, "operational")
	if a == "" || b == "" || c == "" || a == b || b == c {
		t.Errorf("сводные статусы должны отличаться и быть непустыми: %q/%q/%q", a, b, c)
	}
}

// TestChannelKindLabel — известные типы каналов локализуются, неизвестный
// возвращается как есть.
func TestChannelKindLabel(t *testing.T) {
	ctx := ruCtx()
	for _, k := range []string{alert.ChannelEmail, alert.ChannelWebhook, alert.ChannelTelegram} {
		if got := channelKindLabel(ctx, k); got == "" || got == k {
			t.Errorf("channelKindLabel(%q) не локализован: %q", k, got)
		}
	}
	if got := channelKindLabel(ctx, "custom"); got != "custom" {
		t.Errorf("неизвестный канал должен вернуться как есть: %q", got)
	}
}

// TestPerfKindLabel — виды perf-issue локализуются, неизвестный как есть.
func TestPerfKindLabel(t *testing.T) {
	ctx := ruCtx()
	for _, k := range []string{trace.KindNPlusOne, trace.KindSlowDBQuery, trace.KindHTTPFlood} {
		if got := perfKindLabel(ctx, k); got == "" || got == k {
			t.Errorf("perfKindLabel(%q) не локализован: %q", k, got)
		}
	}
	if got := perfKindLabel(ctx, "custom"); got != "custom" {
		t.Errorf("неизвестный вид как есть: %q", got)
	}
}

// TestRegressionMetricLabel — duration особый ключ, прочие метрики → «<VITAL>
// p75».
func TestRegressionMetricLabel(t *testing.T) {
	ctx := ruCtx()
	if got := regressionMetricLabel(ctx, "duration"); got == "" || strings.Contains(got, "p75") {
		t.Errorf("duration метка: %q", got)
	}
	if got := regressionMetricLabel(ctx, "lcp"); got != "LCP p75" {
		t.Errorf("lcp метка = %q", got)
	}
}

// TestWindowKindTextKey — еженедельное/разовое окно обслуживания.
func TestWindowKindTextKey(t *testing.T) {
	if windowKindTextKey(uptime.Window{Weekly: true}) != "uptime.maintenance.kind.weekly" {
		t.Error("еженедельное окно")
	}
	if windowKindTextKey(uptime.Window{}) != "uptime.maintenance.kind.oneoff" {
		t.Error("разовое окно")
	}
}

// TestWeekdayLabelKey — дни 0..6 дают разные ключи, вне диапазона — «unknown».
func TestWeekdayLabelKey(t *testing.T) {
	seen := map[string]bool{}
	for d := 0; d <= 6; d++ {
		k := weekdayLabelKey(d)
		if k == "uptime.weekday.unknown" {
			t.Errorf("день %d не должен быть unknown", d)
		}
		seen[k] = true
	}
	if len(seen) != 7 {
		t.Errorf("ожидалось 7 уникальных ключей дней, got %d", len(seen))
	}
	if weekdayLabelKey(-1) != "uptime.weekday.unknown" || weekdayLabelKey(7) != "uptime.weekday.unknown" {
		t.Error("вне диапазона должен быть unknown")
	}
}

// TestMemberRoleLabelKey — ключи ролей owner/admin/member.
func TestMemberRoleLabelKey(t *testing.T) {
	cases := map[org.Role]string{
		org.RoleOwner:  "org.role.owner",
		org.RoleAdmin:  "org.role.admin",
		org.RoleMember: "org.role.member",
	}
	for in, want := range cases {
		if got := memberRoleLabelKey(in); got != want {
			t.Errorf("memberRoleLabelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMonitorToggleHelpers — включённый монитор предлагает «пауза» и целится в
// pause-путь, выключенный — «возобновить» и resume-путь.
func TestMonitorToggleHelpers(t *testing.T) {
	on := uptime.Monitor{ID: 3, Enabled: true}
	off := uptime.Monitor{ID: 3, Enabled: false}
	if monitorToggleLabelKey(on) != "uptime.detail.action_pause" || monitorToggleLabelKey(off) != "uptime.detail.action_resume" {
		t.Error("подпись переключателя монитора")
	}
	if monitorToggleAction(on) == monitorToggleAction(off) {
		t.Error("pause и resume должны вести на разные пути")
	}
}

// TestMonitorFormHelpers — режим правки против создания в заголовке и action.
func TestMonitorFormHelpers(t *testing.T) {
	edit := MonitorFormData{IsEdit: true, MonitorID: 9, ProjectID: 7}
	create := MonitorFormData{ProjectID: 7}
	if monitorFormTitleKey(edit) != "uptime.form.title_edit" || monitorFormTitleKey(create) != "uptime.form.title_new" {
		t.Error("заголовок формы монитора")
	}
	if monitorFormAction(edit) == monitorFormAction(create) {
		t.Error("правка и создание должны слать на разные action")
	}
}

// TestRuleScope — область правила: пустой env даёт «все», label добавляется
// хвостом.
func TestRuleScope(t *testing.T) {
	ctx := ruCtx()
	all := ruleScope(ctx, metric.Rule{})
	if all == "" {
		t.Error("пустая область должна дать локализованное «все»")
	}
	scoped := ruleScope(ctx, metric.Rule{Environment: "production", LabelKey: "route", LabelValue: "/a"})
	if !strings.Contains(scoped, "production") || !strings.Contains(scoped, "route=/a") {
		t.Errorf("область с env и label: %q", scoped)
	}
}

// TestProfileRegHelpers — форматтеры регрессий профилей: доля, диапазон,
// прирост, «сервис · тип», пустой сервис.
func TestProfileRegHelpers(t *testing.T) {
	if profileRegShare(0.257) != "25.7%" {
		t.Error("доля профиля")
	}
	r := profile.Regression{Service: "web", ProfileType: "cpu", BaselineShare: 0.1, PeakShare: 0.3}
	if profileRegServiceType(r) != "web · cpu" {
		t.Error("сервис·тип")
	}
	if got := profileRegRange(r); got != "10.0% → 30.0%" {
		t.Errorf("диапазон доли = %q", got)
	}
	if got := profileRegIncreasePct(r); got != "+200%" {
		t.Errorf("прирост доли = %q", got)
	}
	if profileRegIncreasePct(profile.Regression{}) != "—" {
		t.Error("нулевая база даёт —")
	}
	if profileDisplayService("") != "(unknown)" || profileDisplayService("web") != "web" {
		t.Error("отображаемое имя сервиса")
	}
}

// TestSslExpiryText — без сертификата «—», просроченный и валидный дают разный
// локализованный текст.
func TestSslExpiryText(t *testing.T) {
	ctx := ruCtx()
	if sslExpiryText(ctx, nil) != "—" {
		t.Error("без сертификата должно быть —")
	}
	past := time.Now().Add(-48 * time.Hour)
	future := time.Now().Add(240 * time.Hour)
	expired := sslExpiryText(ctx, &past)
	valid := sslExpiryText(ctx, &future)
	if expired == "—" || valid == "—" || expired == valid {
		t.Errorf("просроченный/валидный текст должны отличаться: %q vs %q", expired, valid)
	}
}

// TestWindowScheduleText — расписание окна: еженедельное с днём/временем и
// разовое с датами (в т.ч. без дат — «?»).
func TestWindowScheduleText(t *testing.T) {
	ctx := ruCtx()
	weekly := windowScheduleText(ctx, uptime.Window{Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "UTC"})
	if !strings.Contains(weekly, "02:00") || !strings.Contains(weekly, "UTC") {
		t.Errorf("еженедельное расписание: %q", weekly)
	}
	now := time.Now()
	oneoff := windowScheduleText(ctx, uptime.Window{StartsAt: &now, EndsAt: &now, Timezone: "UTC"})
	if !strings.Contains(oneoff, "→") {
		t.Errorf("разовое расписание: %q", oneoff)
	}
	// Без дат — знаки вопроса.
	unknown := windowScheduleText(ctx, uptime.Window{Timezone: "UTC"})
	if !strings.Contains(unknown, "?") {
		t.Errorf("окно без дат должно показать ?: %q", unknown)
	}
}

// TestPathHelpers — базовые маршруты содержат идентификаторы (в т.ч. tracePath
// и incidentsPath, не покрытые рендером страниц).
func TestPathHelpers(t *testing.T) {
	if !strings.Contains(tracePath("abc123"), "abc123") {
		t.Error("tracePath должен нести trace id")
	}
	if !strings.Contains(incidentsPath(7), "7") {
		t.Error("incidentsPath должен нести project id")
	}
}

// TestEnumRenderBadges — компоненты-бейджи рендерят каждое значение своего
// enum с ожидаемым классом/содержимым.
func TestEnumRenderBadges(t *testing.T) {
	// vitalRatingBadge: рейтинг → бейдж с классом; пустой рейтинг ничего не рисует.
	for rating, cls := range map[string]string{"good": "badge-good", "needs-improvement": "badge-warn", "poor": "badge-danger"} {
		out := renderTo(t, vitalRatingBadge(rating))
		if !strings.Contains(out, cls) {
			t.Errorf("vitalRatingBadge(%q) не содержит %q: %s", rating, cls, out)
		}
	}
	if out := renderTo(t, vitalRatingBadge("")); strings.Contains(out, "badge") {
		t.Errorf("пустой рейтинг не должен рисовать бейдж: %s", out)
	}

	// incidentStatusBadge (метрики): open красный, resolved зелёный.
	if !strings.Contains(renderTo(t, incidentStatusBadge("open")), "badge-danger") {
		t.Error("incidentStatusBadge(open) должен быть danger")
	}
	if !strings.Contains(renderTo(t, incidentStatusBadge("resolved")), "badge-good") {
		t.Error("incidentStatusBadge(resolved) должен быть good")
	}

	// ruleEnabledBadge: включённое/выключенное правило.
	on := renderTo(t, ruleEnabledBadge(true))
	off := renderTo(t, ruleEnabledBadge(false))
	if on == off {
		t.Error("бейдж включённости правила должен отличаться")
	}

	// channelStatusBadge: включённый/выключенный канал.
	onCh := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true}))
	offCh := renderTo(t, channelStatusBadge(alert.Channel{Enabled: false}))
	if onCh == offCh {
		t.Error("бейдж включённости канала должен отличаться")
	}
}
