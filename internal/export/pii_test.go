package export

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
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

// TestMaskTagsHidesDenylistedTags — денилист тегов совпадает с request/
// contexts: секрет и email/IP пользователя, пришедший ТЕГОМ (не отдельным
// полем), маскируются наравне с прочими поверхностями PII (P2-SEC-2 аудита).
func TestMaskTagsHidesDenylistedTags(t *testing.T) {
	tags := map[string]string{
		"user.email": "victim@example.com",
		"user_ip":    "203.0.113.7",
		"api_token":  "secret-abc",
		"env":        "prod",
	}
	got := MaskTags(tags)
	for _, k := range []string{"user.email", "user_ip", "api_token"} {
		if got[k] == tags[k] {
			t.Errorf("тег %q = %q, не замаскирован", k, got[k])
		}
	}
	if got["env"] != "prod" {
		t.Errorf("безобидный тег env испорчен: %q", got["env"])
	}
}

// TestMaskTagsDoesNotMutateInput — ScrubTags мутирует карту НА МЕСТЕ, а
// MaskTags обязана работать на копии: вызывающий (eventSource.toRecord)
// продолжает владеть картой event.Stored.Tags, и маскирование выгрузки не
// должно быть видно никому, кто читает те же теги после возврата.
func TestMaskTagsDoesNotMutateInput(t *testing.T) {
	original := map[string]string{"user.email": "victim@example.com", "env": "prod"}
	snapshot := map[string]string{"user.email": "victim@example.com", "env": "prod"}

	got := MaskTags(original)

	for k, wantV := range snapshot {
		if original[k] != wantV {
			t.Fatalf("входная карта изменена: original[%q] = %q, want %q", k, original[k], wantV)
		}
	}
	if len(original) != len(snapshot) {
		t.Fatalf("входная карта изменена по составу ключей: %v, want %v", original, snapshot)
	}
	// Возврат обязан быть НОВОЙ картой, а не тем же объектом.
	got["env"] = "изменено-в-копии"
	if original["env"] != "prod" {
		t.Fatalf("правка результата задела входную карту: original[env] = %q", original["env"])
	}
}

// TestMaskTagsEmptyAndNil — пустая/nil карта не должна падать и не должна
// превращаться в ненужную аллокацию новой пустой карты (нечего копировать —
// нечего маскировать).
func TestMaskTagsEmptyAndNil(t *testing.T) {
	if got := MaskTags(nil); got != nil {
		t.Errorf("MaskTags(nil) = %#v, want nil", got)
	}
	empty := map[string]string{}
	if got := MaskTags(empty); len(got) != 0 {
		t.Errorf("MaskTags({}) = %#v, want пустую карту", got)
	}
}

// TestMaskJSONConcurrentUseIsRace-free — jsonScrubber теперь один общий
// *ingest.Scrubber на весь пакет (см. её докблок в pii.go), а не новый на
// каждый вызов MaskJSON: eventSource.toRecord зовёт её из воркера, который в
// проде обрабатывает заявки одну за другой, но тест не должен опираться на
// это — сама смена «новый Scrubber на вызов» → «общий на пакет» обязана быть
// безопасной при параллельном вызове из нескольких горутин НЕЗАВИСИМО от
// того, использует ли её в проде сейчас больше одной. Гоняется с -race.
func TestMaskJSONConcurrentUseIsRaceFree(t *testing.T) {
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			raw := `{"headers":{"Authorization":"Bearer ` + strconv.Itoa(i) + `"},"n":` + strconv.Itoa(i) + `}`
			got := MaskJSON(raw)
			if strings.Contains(got, "Bearer "+strconv.Itoa(i)) {
				t.Errorf("goroutine %d: секрет не замаскирован: %s", i, got)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Errorf("goroutine %d: MaskJSON вернул невалидный JSON: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
