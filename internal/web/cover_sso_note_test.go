package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebOrgSettingsSSONoteForNonAdmin — владелец орга, НЕ являющийся админом
// инстанса, видит в настройках статус SSO и примечание «настраивает админ
// инстанса», но НЕ форму (CanConfigure=false). Инстанс-админ — форму. Покрывает
// обе ветки видимости SSO-секции.
func TestWebOrgSettingsSSONoteForNonAdmin(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// Первый зарегистрированный юзер становится инстанс-админом (bootstrap) —
	// регистрируем его отдельно, чтобы наш владелец был ОБЫЧНЫМ (не инстанс-админ).
	orgSettingsRegister(t, authSvc, "sso-note-bootstrap@example.com")
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "sso-note-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "sso-note-co", "SSO Note Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	settings := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	// Не-инстанс-админ владелец: секция SSO видна, но с примечанием, без формы.
	resp := getWithCookie(t, s.srv, settings, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner settings status = %d, want 200", resp.StatusCode)
	}
	bs := string(body)
	if !strings.Contains(bs, "администратор инстанса") {
		t.Fatalf("non-admin owner: SSO note missing: %s", bs)
	}
	if strings.Contains(bs, `name="issuer"`) {
		t.Fatalf("non-admin owner must NOT see SSO issuer form field")
	}

	// Делаем владельца инстанс-админом (снимая флаг у bootstrap, т.к. админ один):
	// теперь видна форма (поле issuer).
	if _, err := s.pool.Exec(ctx, "UPDATE users SET is_instance_admin = (id = $1)", ownerID); err != nil {
		t.Fatalf("promote owner: %v", err)
	}
	resp = getWithCookie(t, s.srv, settings, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="issuer"`) {
		t.Fatalf("instance-admin owner must see SSO issuer form field: %s", body)
	}
}
