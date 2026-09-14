package templates

import (
	"strings"
	"testing"
)

// Видимый текст ссылки — инициалы ("WV"), не имя пользователя: без отдельного
// доступного имени скринридер озвучивал бы "ссылка, ДЕ" вместо email.
func TestAvatarLinkHasAccessibleName(t *testing.T) {
	var sb strings.Builder
	if err := layout("t", "wvsort-owner@example.com").Render(ruCtx(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, `class="avatar"`) {
		t.Fatalf("нет ссылки на профиль: %s", out)
	}
	if !strings.Contains(out, `aria-label="Профиль: wvsort-owner@example.com"`) {
		t.Errorf("ссылка на профиль без доступного имени (aria-label): %s", out)
	}
}
