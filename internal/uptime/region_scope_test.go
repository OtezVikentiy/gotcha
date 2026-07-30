package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestMonitorRegionMustBeAvailable: форма предлагает только свои регионы, но
// POST принимал любую строку. Монитор в несуществующем регионе попадает в
// очередь, и его не забирает никто — тихий отказ мониторинга, который выглядит
// как «проверок нет, значит всё хорошо».
func TestMonitorRegionMustBeAvailable(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})

	if _, err := svc.Create(ctx, m, []string{"no-such-region"}, nil); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("Create с несуществующим регионом: err = %v, want ErrInvalidMonitor", err)
	}

	// Встроенный регион доступен всегда.
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create со встроенным регионом: %v", err)
	}

	// Тот же инвариант на Update: иначе правка формы обходит проверку.
	if err := svc.Update(ctx, created, []string{"no-such-region"}, nil); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("Update с несуществующим регионом: err = %v, want ErrInvalidMonitor", err)
	}

	// Регион пробы организации доступен.
	orgID := orgOfProject(t, pool, pid)
	if _, _, err := svc.CreateProbe(ctx, orgID, "eu-west", "EU probe"); err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	if err := svc.Update(ctx, created, []string{"eu-west"}, nil); err != nil {
		t.Fatalf("Update с регионом пробы: %v", err)
	}
}
