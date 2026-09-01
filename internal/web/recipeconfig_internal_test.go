package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRecipeConfigUsesServerKey — рецепты сервисов берут ключ типа SERVER, а
// не agent: их сниппеты сознательно НЕ ставят resourcedetection
// (recipes/registry.go), рецепт хост не регистрирует и никогда не
// регистрировал. Выдать ему agent-ключ значило бы дать право регистрации
// источнику, которому оно не нужно, — против самой цели фичи (§7 дизайна).
func TestRecipeConfigUsesServerKey(t *testing.T) {
	pool := testenv.MigratedPG(t)
	orgSvc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	h := &Handler{Org: orgSvc, BaseURL: "https://g.example"}

	rec, ok := recipes.ByID("postgres")
	if !ok {
		t.Fatal("рецепт postgres не найден")
	}

	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id",
		"recipe-config-server@example.com").Scan(&uid); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	o, err := orgSvc.CreateOrg(ctx, "rcs-org", "RCS Org", uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "rcs-proj", "RCS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys, err := orgSvc.CreateKeys(ctx, proj.ID, org.KindServer, org.KindAgent)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	var serverKey, agentKey string
	for _, k := range keys {
		switch k.Kind {
		case org.KindServer:
			serverKey = k.PublicKey
		case org.KindAgent:
			agentKey = k.PublicKey
		}
	}
	if serverKey == "" || agentKey == "" {
		t.Fatalf("keys = %+v, want один server и один agent", keys)
	}

	config := h.recipeConfig(ctx, proj.ID, rec)
	if !strings.Contains(config, serverKey) {
		t.Errorf("config без server-ключа %q: %s", serverKey, config)
	}
	if strings.Contains(config, agentKey) {
		t.Errorf("config содержит agent-ключ %q, ожидался только server: %s", agentKey, config)
	}
}
