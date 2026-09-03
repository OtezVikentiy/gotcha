package templates

import (
	"strings"
	"testing"
	"time"

	// Часовые пояса вкомпилированы в тест: пакет читает time.LoadLocation, но
	// (в отличие от internal/web) не подключает internal/testenv, где обычно
	// живёт этот импорт — так что подключаем сами, иначе в slim-контейнере без
	// /usr/share/zoneinfo тест падает не по вине проверяемого кода.
	_ "time/tzdata"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestFormStateGet — nil-состояние (первое открытие формы) отдаёт значения по
// умолчанию, заполненное перекрывает их.
func TestFormStateGet(t *testing.T) {
	var empty FormState
	if got := empty.Get("window_seconds", "300"); got != "300" {
		t.Errorf("nil-состояние = %q, want дефолт 300", got)
	}
	if empty.Has() {
		t.Error("nil-состояние не должно открывать модалку")
	}

	f := FormState{"window_seconds": "60", "metric_name": "http.rps"}
	if got := f.Get("window_seconds", "300"); got != "60" {
		t.Errorf("введённое значение = %q, want 60", got)
	}
	if got := f.Get("environment", ""); got != "" {
		t.Errorf("незаполненное поле = %q, want пусто", got)
	}
	if !f.Has() {
		t.Error("заполненное состояние должно открывать модалку")
	}
	if !f.Selected("aggregation", "avg", "avg") {
		t.Error("невведённый селект должен браться из дефолта")
	}
	if f.Selected("metric_name", "other", "http.rps") {
		t.Error("Selected сравнивает с введённым значением, а не с дефолтом")
	}
}

// TestMetricAlertsKeepsSubmittedValues фиксирует потерю ввода: при ошибке
// валидации страница отрисовывается заново, фрагмента #id в адресе уже нет,
// модалка закрывалась вместе с заполненными полями. Человек заполнял семь
// полей, ошибался в одном и начинал сначала.
//
// Состояние помечено Open: с появлением модалки правки на каждое правило
// признаком открытия служит явный ключ, а не сам факт непустого состояния
// (см. metricRuleCreateModal). Так же 422 собирает обработчик — иначе тест
// проверял бы состояние, которого сервер не отдаёт.
func TestMetricAlertsKeepsSubmittedValues(t *testing.T) {
	form := FormState{
		"metric_name":    "process.memory.usage",
		"aggregation":    "p95",
		"comparator":     "lt",
		"threshold":      "abc",
		"window_seconds": "60",
		"environment":    "staging",
		"label_key":      "route",
		"label_value":    "/api",
	}.Open(MetricRuleCreateModalID)
	out := renderTo(t, MetricAlerts(7, nil, nil, []string{"http.rps"}, form,
		"порог должен быть числом", "u@e.com"))

	// Модалка открыта с сервера — иначе введённое было бы недостижимо.
	if !strings.Contains(out, "modal--open") {
		t.Error("модалка должна открыться с сервера при ошибке валидации")
	}
	// Каждое введённое значение вернулось в форму.
	for name, want := range map[string]string{
		"metric_name":    "process.memory.usage",
		"threshold":      "abc",
		"window_seconds": "60",
		"environment":    "staging",
		"label_key":      "route",
		"label_value":    "/api",
	} {
		if !strings.Contains(out, `value="`+want+`"`) {
			t.Errorf("поле %s потеряло введённое значение %q", name, want)
		}
	}
	// Селекты сохранили выбор.
	if !strings.Contains(out, `<option value="p95" selected>`) {
		t.Error("селект агрегации потерял выбор")
	}
	if !strings.Contains(out, `<option value="lt" selected>`) {
		t.Error("селект сравнения потерял выбор")
	}

	// Первое открытие: состояния нет, модалка закрыта, значения дефолтные.
	fresh := renderTo(t, MetricAlerts(7, []metric.Rule{}, nil, nil, nil, "", "u@e.com"))
	if strings.Contains(fresh, "modal--open") {
		t.Error("без ошибки валидации модалка не должна открываться сама")
	}
	if !strings.Contains(fresh, `value="300"`) {
		t.Error("окно по умолчанию должно быть 300 секунд")
	}

	// 422 из модалки ПРАВКИ не должна открывать модалку создания и тащить в
	// неё чужой ввод: состояние формы на странице одно, а модалок N+1.
	foreign := renderTo(t, MetricAlerts(7, nil, nil, nil,
		FormState{"metric_name": "process.memory.usage"}.Open(EditMetricRuleModalID(42)),
		"порог должен быть числом", "u@e.com"))
	if strings.Contains(foreign, "modal--open") {
		t.Error("модалка создания открылась по метке чужой модалки")
	}
	if strings.Contains(foreign, `value="process.memory.usage"`) {
		t.Error("ввод из модалки правки протёк в форму создания")
	}
}

// TestFormStateOpensPicksOneModal — с появлением правки каналов, команд и окон
// модалок на странице стало по одной на строку таблицы, а состояние формы
// по-прежнему одно. Признаком «открыть» служит явный ключ, а не сам факт
// непустого состояния: иначе ошибка формы правки открывала бы форму создания.
func TestFormStateOpensPicksOneModal(t *testing.T) {
	f := FormState{"target": "not-a-url"}.Open("edit-channel-7")
	if !f.Opens("edit-channel-7") {
		t.Error("помеченная модалка должна открываться")
	}
	if f.Opens("new-channel") || f.Opens("edit-channel-8") {
		t.Error("непомеченные модалки открываться не должны")
	}
	if !f.Has() {
		t.Error("служебный ключ не должен скрывать введённые значения")
	}
	if got := f.Get("target", ""); got != "not-a-url" {
		t.Errorf("введённое значение = %q, want not-a-url", got)
	}

	// Служебный ключ сам по себе не считается введённым значением: иначе
	// пустая помеченная форма выглядела бы заполненной.
	if (FormState{}).Open("new-team").Has() {
		t.Error("одна лишь пометка модалки не должна считаться вводом")
	}
	var nilState FormState
	if nilState.Opens("new-team") {
		t.Error("nil-состояние не открывает ничего")
	}
	if !nilState.Open("new-team").Opens("new-team") {
		t.Error("Open на nil-состоянии должен вернуть рабочую карту")
	}
}

// TestWindowFieldDefaults — значения полей формы для уже сохранённого окна.
// Разовое окно уезжает в datetime-local В ЕГО ПОЯСЕ: поле принимает только
// местное время, и подстановка UTC сдвинула бы показанное окно на смещение
// пояса, а сохранение формы закрепило бы этот сдвиг.
func TestWindowFieldDefaults(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	start := time.Date(2026, 8, 1, 2, 0, 0, 0, msk)
	end := start.Add(2 * time.Hour)
	f := windowFieldDefaults(uptime.Window{
		ID: 1, Name: "DB upgrade", StartsAt: &start, EndsAt: &end, Timezone: "Europe/Moscow",
	})
	if got := f.Get("starts_at", ""); got != "2026-08-01T02:00" {
		t.Errorf("starts_at = %q, want местное 2026-08-01T02:00", got)
	}
	if got := f.Get("kind", ""); got != "oneoff" {
		t.Errorf("kind = %q, want oneoff", got)
	}
	if got := f.Get("timezone", ""); got != "Europe/Moscow" {
		t.Errorf("timezone = %q, want Europe/Moscow (есть в списке)", got)
	}

	// Еженедельное окно в поясе, которого нет в фиксированном списке: select
	// переключается на «Другой» (пустое значение), пояс уезжает в своё поле.
	f = windowFieldDefaults(uptime.Window{
		ID: 2, Name: "Nightly", Weekly: true, Weekday: 3,
		StartTime: "01:00", EndTime: "02:00", Timezone: "Asia/Tokyo",
	})
	if got := f.Get("kind", ""); got != "weekly" {
		t.Errorf("kind = %q, want weekly", got)
	}
	if got := f.Get("weekday", ""); got != "3" {
		t.Errorf("weekday = %q, want 3", got)
	}
	if got := f.Get("timezone", "UTC"); got != "" {
		t.Errorf("timezone = %q, want пусто (выбор «Другой»)", got)
	}
	if got := f.Get("timezone_custom", ""); got != "Asia/Tokyo" {
		t.Errorf("timezone_custom = %q, want Asia/Tokyo", got)
	}
	if _, ok := f["starts_at"]; ok {
		t.Error("у еженедельного окна не должно быть starts_at")
	}
}

// TestMaintenanceEditModalPrefilled — модалка правки отрисовывает поля окна, а
// не пустую форму, и остаётся закрытой, пока состояние формы указывает на
// другую модалку.
func TestMaintenanceEditModalPrefilled(t *testing.T) {
	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w := uptime.Window{ID: 42, ProjectID: 7, Name: "DB upgrade", StartsAt: &start, EndsAt: &end, Timezone: "UTC"}

	html := renderTo(t, Maintenance(7, []uptime.Window{w}, nil, "", "u@example.com"))
	if !strings.Contains(html, `id="edit-window-42"`) {
		t.Fatalf("нет модалки правки:\n%s", html)
	}
	if !strings.Contains(html, `value="2026-08-01T02:00"`) {
		t.Fatalf("поля модалки не заполнены значениями окна:\n%s", html)
	}
	if strings.Contains(html, `id="edit-window-42" class="modal modal--open"`) {
		t.Fatal("модалка правки не должна открываться сама по себе")
	}

	// Ошибка формы СОЗДАНИЯ не открывает модалку правки и не подменяет её поля.
	html = renderTo(t, Maintenance(7, []uptime.Window{w},
		FormState{"name": "Черновик"}.Open("new-maintenance-window"), "Неверное окно", "u@example.com"))
	if !strings.Contains(html, `id="new-maintenance-window" class="modal modal--open"`) {
		t.Fatalf("форма создания не открыта:\n%s", html)
	}
	if strings.Contains(html, `id="edit-window-42" class="modal modal--open"`) {
		t.Fatal("модалка правки открылась вместе с формой создания")
	}
	if !strings.Contains(html, `value="DB upgrade"`) {
		t.Fatal("модалка правки потеряла значения окна из-за состояния чужой формы")
	}
}
