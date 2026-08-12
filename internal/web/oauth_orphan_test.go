package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Находка P1-4: в open- и invite-ветках oauthProvision CreateOAuthUser
// создаёт аккаунт ДО LinkIdentity; если LinkIdentity падает, аккаунт
// оставался висячим — занимал email, а войти под ним было нельзя ни паролем
// (OAuth-юзер его не получает), ни этим же провайдером (identity не
// привязана).
//
// LinkIdentity падает ErrIdentityTaken только когда (provider,subject) успел
// занять ДРУГОЙ аккаунт МЕЖДУ проверкой шага 1 «вход по стабильному
// субъекту» (oauthCallback, до вызова oauthProvision) и самим вызовом
// LinkIdentity внутри oauthProvision — то есть только в гонке двух
// параллельных первых входов одним и тем же внешним identity. Через полный
// HTTP-коллбэк это не воспроизвести детерминированно (шаг 1 сам находит уже
// привязанный subject и логинит им, не долетая до oauthProvision). Поэтому
// тесты зовут oauthProvision НАПРЯМУЮ, как эквивалент "гонка уже случилась,
// LinkIdentity видит PK-конфликт" — тот же код, тот же откат, без хрупкой
// синхронизации двух горутин.
func newOrphanTestHandler(t *testing.T) (*Handler, *auth.Service, *org.Service) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := New(authSvc, orgSvc, nil, nil, "http://localhost:8080")
	return h, authSvc, orgSvc
}

func TestOAuthOpenProvisionRollsBackOrphanOnLinkIdentityFailure(t *testing.T) {
	h, authSvc, _ := newOrphanTestHandler(t)
	ctx := context.Background()
	h.RegistrationMode = "open"

	otherUID, err := authSvc.Register(ctx, "other-open@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	if err := authSvc.LinkIdentity(ctx, otherUID, "oidc", "sub-taken-open", "other-open@corp.com"); err != nil {
		t.Fatalf("pre-link identity: %v", err)
	}

	const newEmail = "orphan-open@corp.com"
	id := oauth.Identity{Subject: "sub-taken-open", Email: newEmail, EmailVerified: true}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/oidc/callback", nil)
	h.oauthProvision(rec, req, "oidc", id)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (LinkIdentity must fail on PK conflict)", rec.Code)
	}
	if _, err := authSvc.UserByEmail(ctx, newEmail); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("UserByEmail(%q) err = %v, want ErrUserNotFound — orphaned account was not rolled back", newEmail, err)
	}
}

func TestOAuthInviteProvisionRollsBackOrphanOnLinkIdentityFailure(t *testing.T) {
	h, authSvc, orgSvc := newOrphanTestHandler(t)
	ctx := context.Background()
	h.RegistrationMode = "invite"

	ownerID, err := authSvc.Register(ctx, "orphan-owner@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	o, err := orgSvc.CreateOrg(ctx, "orphan-co", "Orphan Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	const newEmail = "orphan-invite@corp.com"
	if _, err := orgSvc.Invite(ctx, o.ID, newEmail, org.RoleAdmin); err != nil {
		t.Fatalf("invite: %v", err)
	}

	otherUID, err := authSvc.Register(ctx, "other-invite@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	if err := authSvc.LinkIdentity(ctx, otherUID, "oidc", "sub-taken-invite", "other-invite@corp.com"); err != nil {
		t.Fatalf("pre-link identity: %v", err)
	}

	id := oauth.Identity{Subject: "sub-taken-invite", Email: newEmail, EmailVerified: true}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/oidc/callback", nil)
	h.oauthProvision(rec, req, "oidc", id)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (LinkIdentity must fail on PK conflict)", rec.Code)
	}
	if _, err := authSvc.UserByEmail(ctx, newEmail); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("UserByEmail(%q) err = %v, want ErrUserNotFound — orphaned account was not rolled back", newEmail, err)
	}
	// F2 (реордер Link→Accept): сбой LinkIdentity НЕ должен гасить приглашение.
	// Инвайт принимается только после успешной привязки, поэтому accepted_at не
	// выставлен и приглашённый может войти повторно без переприглашения. До
	// реордера Accept шёл первым, помечал инвайт принятым, а откат юзера его в
	// pending не возвращал — приглашение «сгорало».
	if has, err := orgSvc.HasPendingInvite(ctx, newEmail); err != nil {
		t.Fatalf("HasPendingInvite(%q): %v", newEmail, err)
	} else if !has {
		t.Fatalf("invite for %q was consumed by a failed LinkIdentity — must stay pending", newEmail)
	}
}
