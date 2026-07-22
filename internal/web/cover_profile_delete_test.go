package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebProfileDelete — самоудаление аккаунта (M5): единственный владелец орга
// блокируется (409), обычный участник проходит двухшаговое подтверждение и
// удаляется; после удаления сессия недействительна.
func TestWebProfileDelete(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// owner — единственный владелец организации → удаление запрещено (409).
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "pdel-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "pdel-org", "PDel", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	resp := postForm(t, s.srv, "/profile/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("owner delete: status = %d, want 409 (sole owner)", resp.StatusCode)
	}

	// member — не владелец, может удалиться.
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "pdel-member@example.com")
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Без confirmed — страница подтверждения (200), удаления ещё нет.
	resp = postForm(t, s.srv, "/profile/delete", url.Values{}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member delete (confirm step): status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "/profile/delete") {
		t.Fatalf("confirm page missing delete action form: %s", body)
	}

	// confirmed=yes — удаление, редирект на /login.
	resp = postForm(t, s.srv, "/profile/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("member delete (confirmed): status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}

	// Аккаунт удалён: старая сессия больше не пускает на /profile.
	resp = getWithCookie(t, s.srv, "/profile", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("deleted user's session still valid on /profile (status 200)")
	}
}
