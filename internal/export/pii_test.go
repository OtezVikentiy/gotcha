package export

import (
	"encoding/json"
	"strings"
	"testing"
)

// MaskUser: непустые ip/email прячутся за маску, пустые остаются пустыми —
// иначе выгрузка честно не отличает «данные скрыты» от «данных не было».
func TestMaskUserHidesEmailAndIP(t *testing.T) {
	ip, email := MaskUser("203.0.113.7", "user@example.com")
	if ip != maskedValue || email != maskedValue {
		t.Fatalf("MaskUser вернул %q,%q — ожидали маску в обоих", ip, email)
	}
	if ip, email := MaskUser("", ""); ip != "" || email != "" {
		t.Errorf("MaskUser на пустых вернул %q,%q", ip, email)
	}
}

// MaskUser: краевые формы значений — маска не должна зависеть от их вида.
func TestMaskUserEdgeCases(t *testing.T) {
	cases := []struct {
		name, ip, email string
	}{
		{"без @ в email", "203.0.113.7", "не-email-строка"},
		{"несколько @", "203.0.113.7", "a@b@example.com"},
		{"кириллица в локальной части", "203.0.113.7", "иван@пример.рф"},
		{"короткая локальная часть", "203.0.113.7", "a@b.co"},
		{"длинное значение", "203.0.113.7", strings.Repeat("x", 500) + "@example.com"},
		{"верхний регистр", "203.0.113.7", "USER@EXAMPLE.COM"},
		{"IPv6", "2001:db8::1", "user@example.com"},
		{"значение с пробелами", " 203.0.113.7 ", "user @example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, email := MaskUser(c.ip, c.email)
			if ip != maskedValue {
				t.Errorf("%s: ip = %q, хотим %q", c.name, ip, maskedValue)
			}
			if email != maskedValue {
				t.Errorf("%s: email = %q, хотим %q", c.name, email, maskedValue)
			}
			// Маска не должна утекать длину или содержимое исходника.
			if strings.Contains(maskedValue, c.email) && c.email != "" {
				t.Errorf("%s: маска содержит исходный email", c.name)
			}
		})
	}
}

// MaskUser идемпотентен: повторное маскирование уже замаскированного
// значения не портит и не меняет его.
func TestMaskUserIdempotent(t *testing.T) {
	ip, email := MaskUser("203.0.113.7", "user@example.com")
	ip2, email2 := MaskUser(ip, email)
	if ip2 != maskedValue || email2 != maskedValue {
		t.Fatalf("повторное MaskUser дало %q,%q", ip2, email2)
	}
}

func TestMaskJSONHidesSensitiveHeaders(t *testing.T) {
	raw := `{"headers":{"Authorization":"Bearer abc","X-Api-Key":"k","Accept":"*/*"},
	         "url":"https://app/x","cookies":"sid=1"}`
	got := MaskJSON(raw)
	for _, secret := range []string{"Bearer abc", `"k"`, "sid=1"} {
		if strings.Contains(got, secret) {
			t.Errorf("в замаскированном JSON осталось %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "*/*") {
		t.Errorf("безобидный заголовок Accept потерян: %s", got)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("MaskJSON вернул невалидный JSON: %v", err)
	}
}

func TestMaskJSONPassesThroughBrokenInput(t *testing.T) {
	if got := MaskJSON(""); got != "" {
		t.Errorf("MaskJSON(\"\") = %q", got)
	}
	if got := MaskJSON("{не json"); got != "{не json" {
		t.Errorf("битый вход изменён: %q", got)
	}
}

// MaskJSON идемпотентен: второй проход по уже замаскированному значению
// не превращает [scrubbed] в мусор и не разваливает JSON.
func TestMaskJSONIdempotent(t *testing.T) {
	raw := `{"headers":{"Authorization":"Bearer abc"},"url":"https://app/x"}`
	once := MaskJSON(raw)
	twice := MaskJSON(once)
	if once != twice {
		t.Fatalf("повторный MaskJSON изменил результат: %q -> %q", once, twice)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(twice), &m); err != nil {
		t.Fatalf("MaskJSON^2 вернул невалидный JSON: %v", err)
	}
}

// MaskJSON применяется одинаково к contexts и к request — оба поля упомянуты
// в спеке как обязательные к маскированию, и оба идут через один и тот же
// денилист без отдельной ветки на "это contexts" / "это request".
func TestMaskJSONCoversRequestAndContexts(t *testing.T) {
	raw := `{"request":{"headers":{"Cookie":"sid=1"}},"contexts":{"trace":{"token":"secret"}}}`
	got := MaskJSON(raw)
	for _, secret := range []string{"sid=1", "secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("не замаскировано %q: %s", secret, got)
		}
	}
}
