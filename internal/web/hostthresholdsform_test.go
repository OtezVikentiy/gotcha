package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
)

// parseThresholdsRequest строит GET-запрос с query-параметрами формы —
// parseHostThresholdsForm читает через r.FormValue, которому всё равно,
// пришли ли значения из query или из тела POST (тот же приём избавляет от
// Content-Type-заголовка multipart/urlencoded в тесте).
func parseThresholdsRequest(t *testing.T, form url.Values) *http.Request {
	t.Helper()
	return httptest.NewRequest("GET", "/x?"+form.Encode(), nil)
}

// TestParseHostThresholdsFormInherit — форма без единого поля (или с
// нераспознанным режимом) — все 8 указателей nil, каждый вид "наследует"
// (docblock parseHostThresholdsForm).
func TestParseHostThresholdsFormInherit(t *testing.T) {
	ov, err := parseHostThresholdsForm(parseThresholdsRequest(t, url.Values{}))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ov.DiskEnabled != nil || ov.DiskThreshold != nil ||
		ov.MemoryEnabled != nil || ov.MemoryThreshold != nil ||
		ov.LoadEnabled != nil || ov.LoadThreshold != nil ||
		ov.SilentEnabled != nil || ov.SilentAfter != nil {
		t.Errorf("ov = %+v, want все поля nil (inherit)", ov)
	}

	// Нераспознанный режим — тот же inherit, что и пустой (комментарий
	// parseHostThresholdsForm: "любой другой/пустой режим трактуется как
	// inherit").
	form := url.Values{"disk_mode": {"bogus"}}
	ov, err = parseHostThresholdsForm(parseThresholdsRequest(t, form))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ov.DiskEnabled != nil {
		t.Errorf("disk_mode=bogus дал override, want inherit: %+v", ov)
	}
}

// TestParseHostThresholdsFormOverride — режим "override" по каждому из 4
// видов: Enabled=true + распарсенное значение (проценты диска/памяти —
// доля, load — как есть, silent — минуты в time.Duration).
func TestParseHostThresholdsFormOverride(t *testing.T) {
	form := url.Values{
		"disk_mode": {"override"}, "disk_value": {"75"},
		"memory_mode": {"override"}, "memory_value": {"60"},
		"load_mode": {"override"}, "load_value": {"2.5"},
		"silent_mode": {"override"}, "silent_value": {"15"},
	}
	ov, err := parseHostThresholdsForm(parseThresholdsRequest(t, form))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ov.DiskEnabled == nil || !*ov.DiskEnabled || ov.DiskThreshold == nil || *ov.DiskThreshold != 0.75 {
		t.Errorf("disk override = %+v, want enabled=true value=0.75", ov.DiskEnabled)
	}
	if ov.MemoryEnabled == nil || !*ov.MemoryEnabled || ov.MemoryThreshold == nil || *ov.MemoryThreshold != 0.60 {
		t.Errorf("memory override = %+v, want enabled=true value=0.60", ov.MemoryEnabled)
	}
	if ov.LoadEnabled == nil || !*ov.LoadEnabled || ov.LoadThreshold == nil || *ov.LoadThreshold != 2.5 {
		t.Errorf("load override = %+v, want enabled=true value=2.5", ov.LoadEnabled)
	}
	if ov.SilentEnabled == nil || !*ov.SilentEnabled || ov.SilentAfter == nil || *ov.SilentAfter != 15*time.Minute {
		t.Errorf("silent override = %+v, want enabled=true value=15m", ov.SilentEnabled)
	}
}

// TestParseHostThresholdsFormOff — режим "off" по каждому виду: Enabled=false,
// значение не парсится (поле *_value игнорируется, остаётся nil).
func TestParseHostThresholdsFormOff(t *testing.T) {
	form := url.Values{
		"disk_mode": {"off"}, "memory_mode": {"off"},
		"load_mode": {"off"}, "silent_mode": {"off"},
	}
	ov, err := parseHostThresholdsForm(parseThresholdsRequest(t, form))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ov.DiskEnabled == nil || *ov.DiskEnabled || ov.DiskThreshold != nil {
		t.Errorf("disk off = %+v/%v, want enabled=false value=nil", ov.DiskEnabled, ov.DiskThreshold)
	}
	if ov.MemoryEnabled == nil || *ov.MemoryEnabled || ov.MemoryThreshold != nil {
		t.Errorf("memory off = %+v/%v, want enabled=false value=nil", ov.MemoryEnabled, ov.MemoryThreshold)
	}
	if ov.LoadEnabled == nil || *ov.LoadEnabled || ov.LoadThreshold != nil {
		t.Errorf("load off = %+v/%v, want enabled=false value=nil", ov.LoadEnabled, ov.LoadThreshold)
	}
	if ov.SilentEnabled == nil || *ov.SilentEnabled || ov.SilentAfter != nil {
		t.Errorf("silent off = %+v/%v, want enabled=false value=nil", ov.SilentEnabled, ov.SilentAfter)
	}
}

// TestParseHostThresholdsFormInvalid — по каждому виду: нечисловой ввод (или
// пустая строка при override) и NaN/переполнение возвращают тот же
// сентинел-ошибку host.ErrInvalid*, что host.Validate/ValidateOverride —
// hostSettingsErrorMessage не различает источник ошибки (docblock
// parseHostThresholdsForm).
func TestParseHostThresholdsFormInvalid(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want error
	}{
		{"disk нечисловой", url.Values{"disk_mode": {"override"}, "disk_value": {"abc"}}, host.ErrInvalidDiskThreshold},
		{"disk пусто", url.Values{"disk_mode": {"override"}, "disk_value": {""}}, host.ErrInvalidDiskThreshold},
		{"disk NaN", url.Values{"disk_mode": {"override"}, "disk_value": {"NaN"}}, host.ErrInvalidDiskThreshold},
		{"disk +Inf", url.Values{"disk_mode": {"override"}, "disk_value": {"+Inf"}}, host.ErrInvalidDiskThreshold},

		{"memory нечисловой", url.Values{"memory_mode": {"override"}, "memory_value": {"abc"}}, host.ErrInvalidMemoryThreshold},
		{"memory NaN", url.Values{"memory_mode": {"override"}, "memory_value": {"NaN"}}, host.ErrInvalidMemoryThreshold},

		{"load нечисловой", url.Values{"load_mode": {"override"}, "load_value": {"abc"}}, host.ErrInvalidLoadThreshold},
		{"load NaN", url.Values{"load_mode": {"override"}, "load_value": {"NaN"}}, host.ErrInvalidLoadThreshold},

		{"silent нечисловой", url.Values{"silent_mode": {"override"}, "silent_value": {"abc"}}, host.ErrInvalidSilentAfter},
		{"silent отрицательный", url.Values{"silent_mode": {"override"}, "silent_value": {"-5"}}, host.ErrInvalidSilentAfter},
		{"silent за верхней границей (>720 мин)", url.Values{"silent_mode": {"override"}, "silent_value": {"721"}}, host.ErrInvalidSilentAfter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseHostThresholdsForm(parseThresholdsRequest(t, c.form))
			if err == nil {
				t.Fatalf("err = nil, want %v", c.want)
			}
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want wraps %v", err, c.want)
			}
		})
	}
}
