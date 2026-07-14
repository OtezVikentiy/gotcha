package org_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestProjectKeys(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "keys@example.com")
	o, err := svc.CreateOrg(ctx, "keys", "Keys", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	k, err := svc.CreateKey(ctx, p.ID)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(k.PublicKey) {
		t.Fatalf("public key format: %q", k.PublicKey)
	}

	got, err := svc.KeyByPublic(ctx, k.PublicKey)
	if err != nil || got.ProjectID != p.ID {
		t.Fatalf("KeyByPublic: %+v err=%v", got, err)
	}
	if _, err := svc.KeyByPublic(ctx, "00000000000000000000000000000000"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("unknown key: got %v, want ErrNotFound", err)
	}

	if err := svc.RevokeKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := svc.KeyByPublic(ctx, k.PublicKey); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("revoked key resolvable: %v", err)
	}
	keys, err := svc.KeysForProject(ctx, p.ID)
	if err != nil || len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("KeysForProject: %+v err=%v", keys, err)
	}
}
