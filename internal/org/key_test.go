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

	keys, err := svc.CreateKeys(ctx, p.ID, org.KindServer)
	if err != nil {
		t.Fatalf("CreateKeys: %v", err)
	}
	k := keys[0]
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(k.PublicKey) {
		t.Fatalf("public key format: %q", k.PublicKey)
	}

	got, err := svc.KeyByPublic(ctx, k.PublicKey)
	if err != nil || got.ProjectID != p.ID || got.OrgID != o.ID {
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
	keys, err = svc.KeysForProject(ctx, p.ID)
	if err != nil || len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("KeysForProject: %+v err=%v", keys, err)
	}
}

// TestCreateKeysBatchKinds — CreateKeys выпускает несколько ключей ОДНИМ
// запросом (атомарность онбординга: три последовательных вставки утроили бы
// шанс наполовину созданного проекта) и проставляет каждому свой тип.
func TestCreateKeysBatchKinds(t *testing.T) {
	pool := testenv.MigratedPG(t)
	s := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "keys-batch@example.com")
	o, err := s.CreateOrg(ctx, "keysbatch", "KeysBatch", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := s.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	keys, err := s.CreateKeys(ctx, p.ID, org.KindBrowser, org.KindServer, org.KindAgent)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("выпущено %d ключей, ожидалось 3", len(keys))
	}
	want := []org.KeyKind{org.KindBrowser, org.KindServer, org.KindAgent}
	seen := map[string]bool{}
	for i, k := range keys {
		if k.Kind != want[i] {
			t.Errorf("ключ %d: kind=%q, ожидался %q", i, k.Kind, want[i])
		}
		if k.PublicKey == "" || seen[k.PublicKey] {
			t.Errorf("ключ %d: пустой или повторный public_key %q", i, k.PublicKey)
		}
		seen[k.PublicKey] = true
	}

	// Тип доезжает до горячего пути приёма: KeyByPublic его читает.
	got, err := s.KeyByPublic(ctx, keys[2].PublicKey)
	if err != nil {
		t.Fatalf("key by public: %v", err)
	}
	if got.Kind != org.KindAgent {
		t.Fatalf("KeyByPublic вернул kind=%q, ожидался agent", got.Kind)
	}

	// И до страницы настроек: KeysForProject тоже.
	all, err := s.KeysForProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("keys for project: %v", err)
	}
	for _, k := range all {
		if k.Kind == "" {
			t.Fatalf("KeysForProject вернул ключ %d без kind", k.ID)
		}
	}
}

// TestCreateKeysRejectsInvalidKind — недопустимый тип отбивается ДО похода в
// БД: сообщение об ошибке должно быть про тип ключа, а не про нарушение CHECK.
func TestCreateKeysRejectsInvalidKind(t *testing.T) {
	pool := testenv.MigratedPG(t)
	s := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "keys-invalid@example.com")
	o, err := s.CreateOrg(ctx, "keysinvalid", "KeysInvalid", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := s.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := s.CreateKeys(ctx, p.ID, org.KeyKind("root")); !errors.Is(err, org.ErrInvalidKeyKind) {
		t.Fatalf("ожидалась ErrInvalidKeyKind, получено %v", err)
	}
	// Пустой список тоже ошибка: вызов без типов — это забытый аргумент.
	if _, err := s.CreateKeys(ctx, p.ID); !errors.Is(err, org.ErrInvalidKeyKind) {
		t.Fatalf("CreateKeys без типов: ожидалась ErrInvalidKeyKind, получено %v", err)
	}
}
