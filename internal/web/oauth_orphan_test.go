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

// oauthProvision вызывается напрямую: гонка двух параллельных первых входов одним
// identity не воспроизводится детерминированно через HTTP-коллбэк.
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
	if has, err := orgSvc.HasPendingInvite(ctx, newEmail); err != nil {
		t.Fatalf("HasPendingInvite(%q): %v", newEmail, err)
	} else if !has {
		t.Fatalf("invite for %q was consumed by a failed LinkIdentity — must stay pending", newEmail)
	}
}
