package guards

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var formatLiteralRe = regexp.MustCompile(`\.Format\(\s*"([^"]*)"`)

var formatNonLiteralRe = regexp.MustCompile(`\.Format\(\s*([^"\s][^)]*)\)`)

var durationSubStringRe = regexp.MustCompile(`\.Sub\([^()]*\)\.String\(\)`)

// \b перед Duration не проходит внутри более длинного идентификатора
// (myDuration) — ловит только явное имя Duration.
var durationLiteralStringRe = regexp.MustCompile(`\bDuration\.String\(\)`)

var permanentFormatExemptions = []Exemption{
	{Value: ContentAnchor("internal/web/timerange.go", "timeRangeFieldValue", `return t.UTC().Format("2006-01-02T15:04")`), Why: `timeRangeFieldValue: return t.UTC().Format("2006-01-02T15:04") — сериализация value= для <input type="datetime-local">, протокол HTML-формы`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/templates/maintenance.templ", "windowFieldDefaults", `f["starts_at"] = w.StartsAt.In(loc).Format("2006-01-02T15:04")`), Why: `f["starts_at"] = w.StartsAt.In(loc).Format("2006-01-02T15:04") — то же поле формы datetime-local для окна обслуживания`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/templates/maintenance.templ", "windowFieldDefaults", `f["ends_at"] = w.EndsAt.In(loc).Format("2006-01-02T15:04")`), Why: `f["ends_at"] = w.EndsAt.In(loc).Format("2006-01-02T15:04") — то же поле формы datetime-local`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/timerange_test.go", "TestParseTimeRangeStartOnly", `start := now.Add(-48 * time.Hour).Format("2006-01-02T15:04")`), Why: `start := now.Add(-48 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= тем же машинным форматом, что и сама форма, не дублирование человекочитаемого (TestParseTimeRangeStartOnly)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/timerange_test.go", "TestParseCustomRangeClampsFutureEnd", `future := now.Add(48 * time.Hour).Format("2006-01-02T15:04")`), Why: `future := now.Add(48 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра end= (TestParseCustomRangeClampsFutureEnd)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/timerange_test.go", "TestParseCustomRangeClampsFutureEnd", `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04")`), Why: `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= (TestParseCustomRangeClampsFutureEnd)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/timerange_test.go", "TestParseTimeRangeCustomEndDefaultsToNow", `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04")`), Why: `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= (TestParseTimeRangeCustomEndDefaultsToNow)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/performance_test.go", "TestWebEndpointDetailSlowestExpiryConfigurable", `start := now.Add(-45 * 24 * time.Hour).Format("2006-01-02T15:04")`), Why: `start := now.Add(-45 * 24 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра ?start= тем же машинным форматом, что и сама форма (TestWebEndpointDetailSlowestExpiryConfigurable нужен custom-диапазон на 45 дней назад, дефолтные 24ч не захватили бы старые трейсы)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/exports_test.go", "TestExportsCreateHonorsCustomRangeQuery", `"start":  {start.Format("2006-01-02T15:04")},`), Why: `"start": {start.Format("2006-01-02T15:04")} — сборка входного параметра start= тем же машинным форматом, что и TimeRangeVM.apply/<input type="datetime-local"> (TestExportsCreateHonorsCustomRangeQuery)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/exports_test.go", "TestExportsCreateHonorsCustomRangeQuery", `"end":    {end.Format("2006-01-02T15:04")},`), Why: `"end": {end.Format("2006-01-02T15:04")} — тот же входной параметр end=, вторая граница диапазона`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/uptime/window_dst_test.go", "TestWeeklyWindowKeepsDurationAcrossDST", `ivs[0].From.In(berlin).Format("15:04 MST"),`), Why: `ivs[0].From.In(berlin).Format("15:04 MST") — аргумент t.Errorf на строке-продолжении (TestWeeklyWindowKeepsDurationAcrossDST), диагностика для разработчика`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/uptime/window_dst_test.go", "TestWeeklyWindowKeepsDurationAcrossDST", `ivs[0].To.In(berlin).Format("15:04 MST"))`), Why: `ivs[0].To.In(berlin).Format("15:04 MST") — тот же вызов t.Errorf, второе значение`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/uptime/window_dst_test.go", "TestAutumnWindowCoversFirstPass", `firstPass.In(berlin).Format("15:04 MST"),`), Why: `firstPass.In(berlin).Format("15:04 MST") — аргумент t.Fatalf на строке-продолжении (TestAutumnWindowCoversFirstPass)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/uptime/window_dst_test.go", "TestAutumnWindowCoversFirstPass", `ivs[0].From.In(berlin).Format("15:04 MST"),`), Why: `ivs[0].From.In(berlin).Format("15:04 MST") — тот же вызов t.Fatalf`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/uptime/window_dst_test.go", "TestAutumnWindowCoversFirstPass", `ivs[0].To.In(berlin).Format("15:04 MST"))`), Why: `ivs[0].To.In(berlin).Format("15:04 MST") — тот же вызов t.Fatalf, последнее значение`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/ingest/otlp.go", "otlpData", `e["timestamp"] = ts.Format(time.RFC3339Nano)`), Why: `e["timestamp"] = ts.Format(time.RFC3339Nano) — поле экспортируемого OTLP JSON-события, машинный формат для API`, Finding: "по замыслу"},
	{Value: ContentAnchor("cmd/gotcha/health.go", "healthProbe.snapshot", `out["checked_at"] = p.checkedAt.Format(time.RFC3339)`), Why: `out["checked_at"] = p.checkedAt.Format(time.RFC3339) — поле JSON-тела /healthz и /readyz, машинный формат для API-клиента и мониторинга, не для показа человеку`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/ingest/sentry_test.go", "TestParseEventMessageOnly", "}`, want.Format(time.RFC3339Nano))"), Why: `want.Format(time.RFC3339Nano) — тестовый payload в формате Sentry API (TestParseEventMessageOnly)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/ingest/sentry_test.go", "TestParseEventClampsTimestampToWindow", `now.Add(-200*24*time.Hour).Format(time.RFC3339Nano)), now.Add(-maxTimestampAge)},`), Why: `now.Add(-200*24*time.Hour).Format(time.RFC3339Nano) — тестовый payload в формате Sentry API (TestParseEventClampsTimestampToWindow)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/ingest/sentry_test.go", "TestParseEventClampsTimestampToWindow", `inWindow.Format(time.RFC3339Nano))))`), Why: `inWindow.Format(time.RFC3339Nano) — тестовый payload в формате Sentry API`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/ingest/transaction_test.go", "testTransactionRFC3339JSON", `return base.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)`), Why: `base.Add(...).Format(time.RFC3339Nano) — testTransactionRFC3339JSON: часть SDK (sentry-python/старые sentry-php) шлёт timestamps в RFC3339, тестовый payload в том же формате`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/log/parse_ndjson_test.go", "TestParseNDJSONTimestampRFC3339String", "body := fmt.Sprintf(`{\"message\":\"a\",\"timestamp\":%q}`, ts.Format(time.RFC3339))"), Why: `ts.Format(time.RFC3339) — сборка NDJSON-payload с timestamp в RFC3339 (TestParseNDJSONTimestampRFC3339String): вход API логов, машинный формат, не человекочитаемое дублирование`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/log/parse_ndjson_test.go", "TestParseNDJSONTimestampWindowLowerBound", "body := fmt.Sprintf(`{\"message\":\"a\",\"timestamp\":%q}`, tooOld.Format(time.RFC3339))"), Why: `tooOld.Format(time.RFC3339) — NDJSON-payload на now-100d для проверки нижней границы окна (TestParseNDJSONTimestampWindowLowerBound), now-относительное время, литералом не заменить`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/log/parse_ndjson_test.go", "TestParseNDJSONTimestampWindowUpperBound", "body := fmt.Sprintf(`{\"message\":\"a\",\"timestamp\":%q}`, tooNew.Format(time.RFC3339))"), Why: `tooNew.Format(time.RFC3339) — NDJSON-payload на now+48h для проверки верхней границы окна (TestParseNDJSONTimestampWindowUpperBound)`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/web/statuspage.go", "Handler.buildStatusPage", `StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout),`), Why: `StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout) — публичная статус-страница всегда в UTC без JS-локализации, самостоятельный формат по документированному дизайн-решению (см. const statusPageTimeLayout)`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/statuspage.go", "upcomingWindows", `From: ni.iv.From.UTC().Format(statusPageTimeLayout),`), Why: `From: ni.iv.From.UTC().Format(statusPageTimeLayout) — то же дизайн-решение, окно обслуживания`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/web/statuspage.go", "upcomingWindows", `To:   ni.iv.To.UTC().Format(statusPageTimeLayout),`), Why: `To: ni.iv.To.UTC().Format(statusPageTimeLayout) — то же дизайн-решение`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/web/templates/relativetime.templ", "relativeTime", `<time datetime={ t.UTC().Format(time.RFC3339) } title={ humanize.Time(ctx, t, time.UTC) }>`), Why: `<time datetime={ t.UTC().Format(time.RFC3339) }> — машинночитаемый атрибут datetime, не текст для человека (title рядом уже собран через humanize.Time)`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/web/eventdump.go", "writeMeta", `{"time", ev.Timestamp.UTC().Format(time.RFC3339)},`), Why: `ev.Timestamp.UTC().Format(time.RFC3339) — машинный timestamp в LLM-дампе события (текст для модели, не человеческий показ)`, Finding: "по замыслу"},

	// Обход бага биндинга clickhouse-go: позиционный "?" форматирует time.Time
	// с жёстко зашитым TimeUnit=Seconds, теряя миллисекунды параметра молча.
	{Value: ContentAnchor("internal/log/query.go", "chTimeArg", `return t.UTC().Format("2006-01-02 15:04:05.000")`), Why: `t.UTC().Format("2006-01-02 15:04:05.000") — SQL-параметр toDateTime64(?, 3), обход бага биндинга clickhouse-go (TimeUnit=Seconds по умолчанию у позиционных "?"), не человекочитаемый вывод`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/export/writer.go", "cell", `return x.UTC().Format(time.RFC3339)`), Why: `x.UTC().Format(time.RFC3339) — значение time.Time в ячейке файла выгрузки (cell), формат для внешнего парсера файла, не для чтения с экрана`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/web/exports.go", "exportDownloadFilename", `return fmt.Sprintf("gotcha-%s-%s-%s.%s", job.Kind, slug, at.Format("20060102-1504"), job.FileExt)`), Why: `at.Format("20060102-1504") — компонент имени скачиваемого файла (спека §10), протокол именования, не текст на странице`, Finding: "по замыслу"},

	{Value: ContentAnchor("internal/event/query_test.go", "TestStreamForExportOrdersByIssueThenTime", `got = append(got, fmt.Sprintf("%d@%s", ev.IssueID, ev.Timestamp.UTC().Format(time.RFC3339)))`), Why: `ev.Timestamp.UTC().Format(time.RFC3339) — ключ сравнения "issueID@RFC3339" в срезе got, машинный формат для slices.Equal, не человекочитаемый вывод`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/event/query_test.go", "TestStreamForExportOrdersByIssueThenTime", `fmt.Sprintf("%d@%s", issue1, t3.Format(time.RFC3339)),`), Why: `t3.Format(time.RFC3339) — тот же ключ сравнения в срезе want, первая строка`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/event/query_test.go", "TestStreamForExportOrdersByIssueThenTime", `fmt.Sprintf("%d@%s", issue1, t1.Format(time.RFC3339)),`), Why: `t1.Format(time.RFC3339) — тот же ключ сравнения в срезе want, вторая строка`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/event/query_test.go", "TestStreamForExportOrdersByIssueThenTime", `fmt.Sprintf("%d@%s", issue2, t2.Format(time.RFC3339)),`), Why: `t2.Format(time.RFC3339) — тот же ключ сравнения в срезе want, третья строка`, Finding: "по замыслу"},
	{Value: ContentAnchor("internal/event/query_test.go", "TestStreamForExportOrdersByIssueThenTime", `fmt.Sprintf("%d@%s", issue2, t1.Format(time.RFC3339)),`), Why: `t1.Format(time.RFC3339) — тот же ключ сравнения в срезе want, четвёртая строка`, Finding: "по замыслу"},
}

const maxPermanentFormatExemptions = 37

var debtFormatExemptions = []Exemption{
	{Value: ContentAnchor("internal/web/svg.go", "metricTimeLabel", `return t.Format("02.01")`), Why: `return t.Format("02.01") — подпись оси X (короткая дата)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: ContentAnchor("internal/web/svg.go", "metricTimeLabel", `return t.Format("15:04")`), Why: `return t.Format("15:04") — подпись оси X (только время)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: ContentAnchor("internal/web/svg.go", "chartBars", `text := points[idx].T.UTC().Format("02.01")`), Why: `points[idx].T.UTC().Format("02.01") — подпись оси X во flame/vitals-графике`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	{Value: ContentAnchor("internal/web/templates/monitordetail.templ", "sslExpiryText", `date := t.Format("2006-01-02")`), Why: `sslExpiryText: date := t.Format("2006-01-02") — дата истечения TLS-сертификата человеку, вне humanize`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	{Value: ContentAnchor("internal/web/svgaxis.go", "timeAxis", `ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})`), Why: `ticks = append(ticks, xTick{..., text: t.UTC().Format(layout)}) — layout выбран веткой switch из "02.01"/"15:04" (человекочитаемые макеты, та же природа, что долг в svg.go), передан переменной, а не литералом`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	{Value: ContentAnchor("internal/web/svgaxis.go", "writeDeployMarker", `sb.WriteString(html.EscapeString(d.Version + " · " + d.DeployedAt.UTC().Format("02.01 15:04")))`), Why: `html.EscapeString(d.Version + " · " + d.DeployedAt.UTC().Format("02.01 15:04")) — время в <title> маркера деплоя, человекочитаемый макет вне humanize (тот же, что тултипы точек графиков)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
}

const maxDebtFormatExemptions = 6

const minFormatCallsFound = 42

// Ловит только .Format(...)/.Sub().String()/Duration.String() — числовой
// форматтер длительности (fmt.Sprintf/strconv без них) сторож не видит.
func TestNoRawTimeFormattingOutsideHumanize(t *testing.T) {
	tree := Load(t)

	permanentExempt := ExemptedValues(permanentFormatExemptions)
	debtExempt := ExemptedValues(debtFormatExemptions)
	seen := map[string]bool{}
	anchorLines := map[string]int{}
	total := 0

	report := func(path string, line int, fn, fullLine, snippet, why string) {
		anchor := ContentAnchor(path, fn, fullLine)
		recordAnchor(t, "TestNoRawTimeFormattingOutsideHumanize", anchorLines, anchor, line)
		seen[anchor] = true
		total++
		if permanentExempt[anchor] || debtExempt[anchor] {
			return
		}
		t.Errorf("%s:%d: %s вне internal/humanize: %s", path, line, why, snippet)
	}

	for _, f := range tree.GoFiles {
		// _templ.go дублирует свой .templ-исходник (сканируется он сам,
		// ниже) — считать находки дважды под разными путями незачем.
		if f.Generated {
			continue
		}
		if strings.HasPrefix(f.Path, "internal/guards/") || strings.HasPrefix(f.Path, "internal/humanize/") {
			continue
		}
		scanFormatViolations(f.Path, f.Body, report)
	}
	for _, f := range tree.Templates {
		if strings.HasPrefix(f.Path, "internal/guards/") || strings.HasPrefix(f.Path, "internal/humanize/") {
			continue
		}
		scanFormatViolations(f.Path, f.Body, report)
	}

	if total < minFormatCallsFound {
		t.Fatalf("сканер нашёл %d вызовов форматирования времени, ожидалось не меньше %d — либо поломан сам сканер (сузили регулярку, урезали обход), либо дерево честно почищено настолько, что порог пора снижать вместе со списком долга; в обоих случаях сначала разберитесь, какой из двух случаев это, прежде чем трогать minFormatCallsFound", total, minFormatCallsFound)
	}

	CheckExemptions(t, "TestNoRawTimeFormattingOutsideHumanize (по замыслу)", permanentFormatExemptions, maxPermanentFormatExemptions, seen)
	CheckExemptions(t, "TestNoRawTimeFormattingOutsideHumanize (долг задач C7/C8/C9)", debtFormatExemptions, maxDebtFormatExemptions, seen)
}

func scanFormatViolations(path, body string, report func(path string, line int, fn, fullLine, snippet, why string)) {
	fnCtx := funcContexts(body)
	for i, line := range strings.Split(body, "\n") {
		// Полностью закомментированная строка не разбирается вовсе — не
		// только вычищается хвост.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		checked := stripTrailingComment(line)
		fullLine := strings.TrimSpace(checked)
		fn := fnCtx[i]

		for _, m := range formatLiteralRe.FindAllStringSubmatch(checked, -1) {
			report(path, i+1, fn, fullLine, fmt.Sprintf(".Format(%q)", m[1]), "литеральный макет в .Format(")
		}
		for _, m := range formatNonLiteralRe.FindAllStringSubmatch(checked, -1) {
			report(path, i+1, fn, fullLine, fmt.Sprintf(".Format(%s)", m[1]), "нелитеральный аргумент в .Format(")
		}
		for _, m := range durationSubStringRe.FindAllString(checked, -1) {
			report(path, i+1, fn, fullLine, m, ".String() на Duration (X.Sub(Y).String())")
		}
		for _, m := range durationLiteralStringRe.FindAllString(checked, -1) {
			report(path, i+1, fn, fullLine, m, ".String() на Duration (буквальное Duration.String())")
		}
	}
}

func TestFormatCallPatternsRecognizeShapes(t *testing.T) {
	t.Run("formatLiteralRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want string
			ok   bool
		}{
			{"литерал — да", `return t.Format("02.01.2006")`, "02.01.2006", true},
			{"литерал внутри конкатенации — да", `p.T.UTC().Format("02.01 15:04") + " — " + x`, "02.01 15:04", true},
			{"переменная-макет — нет (это ловит formatNonLiteralRe)", `ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})`, "", false},
			{"именованная константа — нет (это тоже ловит formatNonLiteralRe)", `ts.Format(time.RFC3339Nano)`, "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := formatLiteralRe.FindStringSubmatch(tc.line)
				if tc.ok != (m != nil) {
					t.Fatalf("FindStringSubmatch(%q) match=%v, want %v", tc.line, m != nil, tc.ok)
				}
				if tc.ok && m[1] != tc.want {
					t.Errorf("макет = %q, want %q", m[1], tc.want)
				}
			})
		}
	})

	t.Run("formatNonLiteralRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want string
			ok   bool
		}{
			{"переменная-макет — да (internal/web/svgaxis.go:179)", `ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})`, "layout", true},
			{"именованная константа time.RFC3339Nano — да", `ts.Format(time.RFC3339Nano)`, "time.RFC3339Nano", true},
			{"именованный пакетный уровень statusPageTimeLayout — да", `StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout),`, "statusPageTimeLayout", true},
			{"литерал — нет (это ловит formatLiteralRe, не должно дублировать находку)", `return t.Format("02.01.2006")`, "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := formatNonLiteralRe.FindStringSubmatch(tc.line)
				if tc.ok != (m != nil) {
					t.Fatalf("FindStringSubmatch(%q) match=%v, want %v", tc.line, m != nil, tc.ok)
				}
				if tc.ok && m[1] != tc.want {
					t.Errorf("аргумент = %q, want %q", m[1], tc.want)
				}
			})
		}
	})

	t.Run("durationSubStringRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want bool
		}{
			{"Sub().String() — да (мутация задачи: отменённый humanize.Duration)", `return r.ResolvedAt.Sub(r.StartedAt).String()`, true},
			{"Sub() без String() — нет (тот самый безопасный вызов humanize.Duration)", `return humanize.Duration(ctx, r.ResolvedAt.Sub(r.StartedAt))`, false},
			{"String() без Sub() — нет (не имеет отношения к Duration)", `return id.String()`, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := durationSubStringRe.MatchString(tc.line); got != tc.want {
					t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
				}
			})
		}
	})

	t.Run("durationLiteralStringRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want bool
		}{
			{"time.Duration.String() — да", `d := time.Duration.String()`, true},
			{"поле Duration — да", `return x.Duration.String()`, true},
			{"переменная myDuration — нет (нет границы слова перед Duration)", `return myDuration.String()`, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := durationLiteralStringRe.MatchString(tc.line); got != tc.want {
					t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
				}
			})
		}
	})
}

func TestScanFormatViolationsAnchorStableUnderInsertionAbove(t *testing.T) {
	before := "package demo\n\nfunc render(t time.Time) string {\n\treturn t.Format(\"02.01 15:04\")\n}\n"
	after := "package demo\n\n// пустой комментарий-заполнитель на несколько строк,\n// имитирующий безобидную правку выше находки —\n// именно то, что раньше сдвигало exemptLoc.\nfunc noop() {}\n\nfunc render(t time.Time) string {\n\treturn t.Format(\"02.01 15:04\")\n}\n"

	anchorsOf := func(body string) map[string]int {
		out := map[string]int{}
		scanFormatViolations("demo.go", body, func(path string, line int, fn, fullLine, snippet, why string) {
			out[ContentAnchor(path, fn, fullLine)] = line
		})
		return out
	}

	beforeAnchors := anchorsOf(before)
	afterAnchors := anchorsOf(after)
	if len(beforeAnchors) != 1 || len(afterAnchors) != 1 {
		t.Fatalf("ожидалась ровно одна находка до и после вставки, получено %d и %d", len(beforeAnchors), len(afterAnchors))
	}
	var beforeAnchor, afterAnchor string
	var beforeLine, afterLine int
	for a, l := range beforeAnchors {
		beforeAnchor, beforeLine = a, l
	}
	for a, l := range afterAnchors {
		afterAnchor, afterLine = a, l
	}

	if beforeLine == afterLine {
		t.Fatalf("проба ничего не проверяет: вставка строк не сдвинула номер строки находки (осталась %d)", beforeLine)
	}
	if beforeAnchor != afterAnchor {
		t.Fatalf("якорь изменился от вставки строк ВЫШЕ находки: %q -> %q — ContentAnchor обязан быть устойчив к этому", beforeAnchor, afterAnchor)
	}
}

func TestScanFormatViolationsStaleExemptionCaught(t *testing.T) {
	body := "package demo\n\nfunc render(t time.Time) string {\n\treturn t.Format(\"02.01 15:04\")\n}\n"
	seen := map[string]bool{}
	var anchor string
	scanFormatViolations("demo.go", body, func(path string, line int, fn, fullLine, snippet, why string) {
		anchor = ContentAnchor(path, fn, fullLine)
		seen[anchor] = true
	})
	if anchor == "" {
		t.Fatalf("синтетический .Format(...) не найден сканером — проба сломана")
	}
	exempt := []Exemption{{Value: anchor, Why: "проба", Finding: "проба"}}

	ft := &fakeT{}
	CheckExemptions(ft, "проба", exempt, 5, seen)
	if ft.failed {
		t.Fatalf("здоровое исключение забраковано: %v", ft.msgs)
	}

	fixedBody := "package demo\n\nfunc render(t time.Time) string {\n\treturn humanize.Time(t)\n}\n"
	fixedSeen := map[string]bool{}
	scanFormatViolations("demo.go", fixedBody, func(path string, line int, fn, fullLine, snippet, why string) {
		fixedSeen[ContentAnchor(path, fn, fullLine)] = true
	})

	ft2 := &fakeT{}
	CheckExemptions(ft2, "проба", exempt, 5, fixedSeen)
	ft2.requireFailure(t, "устарело")
}

func TestContentAnchorChangesWhenFindingLineItselfChanges(t *testing.T) {
	original := "package demo\n\nfunc render(t time.Time) string {\n\treturn t.Format(\"02.01 15:04\")\n}\n"
	renamed := "package demo\n\nfunc render(when time.Time) string {\n\treturn when.Format(\"02.01 15:04\")\n}\n"

	var origAnchor, renamedAnchor string
	scanFormatViolations("demo.go", original, func(path string, line int, fn, fullLine, snippet, why string) {
		origAnchor = ContentAnchor(path, fn, fullLine)
	})
	scanFormatViolations("demo.go", renamed, func(path string, line int, fn, fullLine, snippet, why string) {
		renamedAnchor = ContentAnchor(path, fn, fullLine)
	})

	if origAnchor == "" || renamedAnchor == "" {
		t.Fatalf("проба сломана: находка не обнаружена (%q, %q)", origAnchor, renamedAnchor)
	}
	if origAnchor == renamedAnchor {
		t.Fatalf("переименование параметра в строке находки (t -> when) обязано менять якорь, а осталось прежним %q — иначе схема прячет реальную правку кода под старым исключением", origAnchor)
	}
}
