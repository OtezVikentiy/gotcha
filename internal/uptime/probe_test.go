package uptime_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// newOrgID creates a bare organization (no project) — probes belong to an
// org, not a project. Reuses the projectSeq counter from monitor_test.go so
// slugs/emails stay globally unique across the package's tests.
func newOrgID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	n := projectSeq.Add(1)
	var orgID int64
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Up',1000000) RETURNING id",
		fmt.Sprintf("probe-org-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	return orgID
}

func TestProbeLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orgID := newOrgID(t, pool)

	probe, token, err := svc.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("CreateProbe: token = %q (len %d), want 64 hex chars", token, len(token))
	}
	if probe.ID == 0 {
		t.Fatalf("CreateProbe: id = 0")
	}
	if probe.Revoked {
		t.Fatalf("CreateProbe: Revoked = true, want false")
	}

	found, err := svc.ProbeByToken(ctx, token)
	if err != nil {
		t.Fatalf("ProbeByToken: %v", err)
	}
	if found.ID != probe.ID || found.Region != "eu-west" || found.Name != "Probe 1" {
		t.Fatalf("ProbeByToken: %+v, want id=%d region=eu-west name=%q", found, probe.ID, "Probe 1")
	}

	if err := svc.TouchProbe(ctx, probe.ID); err != nil {
		t.Fatalf("TouchProbe: %v", err)
	}
	touched, err := svc.ProbeByToken(ctx, token)
	if err != nil {
		t.Fatalf("ProbeByToken after touch: %v", err)
	}
	if touched.LastSeenAt == nil {
		t.Fatalf("TouchProbe: LastSeenAt still nil")
	}

	regions, err := svc.Regions(ctx, orgID)
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	if len(regions) != 2 || regions[0] != "eu-west" || regions[1] != "local" {
		t.Fatalf("Regions = %v, want [eu-west local]", regions)
	}

	if err := svc.RevokeProbe(ctx, probe.ID); err != nil {
		t.Fatalf("RevokeProbe: %v", err)
	}
	if _, err := svc.ProbeByToken(ctx, token); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("ProbeByToken after revoke: err = %v, want ErrNotFound", err)
	}
	if err := svc.RevokeProbe(ctx, probe.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("RevokeProbe again: err = %v, want ErrNotFound", err)
	}

	regionsAfterRevoke, err := svc.Regions(ctx, orgID)
	if err != nil {
		t.Fatalf("Regions after revoke: %v", err)
	}
	if len(regionsAfterRevoke) != 1 || regionsAfterRevoke[0] != "local" {
		t.Fatalf("Regions after revoke = %v, want [local]", regionsAfterRevoke)
	}

	probes, err := svc.Probes(ctx, orgID)
	if err != nil {
		t.Fatalf("Probes: %v", err)
	}
	if len(probes) != 1 || !probes[0].Revoked {
		t.Fatalf("Probes = %+v, want 1 revoked probe", probes)
	}
}

func TestCreateProbeTokensAreUniqueAndOnlyReturnedOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	orgID := newOrgID(t, pool)

	_, token1, err := svc.CreateProbe(ctx, orgID, "eu", "P1")
	if err != nil {
		t.Fatalf("CreateProbe 1: %v", err)
	}
	_, token2, err := svc.CreateProbe(ctx, orgID, "eu", "P2")
	if err != nil {
		t.Fatalf("CreateProbe 2: %v", err)
	}
	if token1 == token2 {
		t.Fatalf("CreateProbe: tokens not unique: %q", token1)
	}

	var rawStored int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM probes WHERE token_hash = $1", []byte(token1)).Scan(&rawStored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if rawStored != 0 {
		t.Fatalf("raw token must not be stored verbatim in token_hash")
	}
}

func TestProbeByUnknownTokenNotFound(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.ProbeByToken(ctx, "does-not-exist"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("ProbeByToken unknown: err = %v, want ErrNotFound", err)
	}
}
