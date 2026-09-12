package host

import "testing"

func TestResolverCascade(t *testing.T) {
	proj := DefaultSettings()
	proj.DiskThreshold = 0.85
	on := true
	off := false
	rDisk := 0.70
	rLoad := 4.0
	groups := []GroupThreshold{
		{Scope: "role", Label: "web", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &rDisk}},
		{Scope: "env", Label: "prod", ThresholdOverride: ThresholdOverride{LoadEnabled: &on, LoadThreshold: &rLoad}},
	}
	hLoad := 6.0
	ovr := map[int64]ThresholdOverride{
		1: {LoadEnabled: &on, LoadThreshold: &hLoad, MemoryEnabled: &off},
	}
	r := ThresholdResolver{Project: proj, ProjectExists: true, Groups: groups, Overrides: ovr}
	h := Host{ID: 1, Environment: "prod", Role: "web"}
	eff := r.Effective(h)
	if eff.Settings.DiskThreshold != 0.70 || eff.DiskSource.Level != "role" || eff.DiskSource.Label != "web" {
		t.Fatalf("disk: %v %+v", eff.Settings.DiskThreshold, eff.DiskSource)
	}
	if eff.Settings.LoadThreshold != 6.0 || eff.LoadSource.Level != "host" {
		t.Fatalf("load: %v %+v", eff.Settings.LoadThreshold, eff.LoadSource)
	}
	if eff.Settings.MemoryEnabled != false || eff.MemorySource.Level != "host" || eff.Settings.MemoryThreshold != 0.90 {
		t.Fatalf("memory: %+v", eff.Settings)
	}
	if eff.SilentSource.Level != "project" {
		t.Fatalf("silent src: %+v", eff.SilentSource)
	}
}

func TestResolverRoleBeatsEnv(t *testing.T) {
	on := true
	roleMem := 0.75
	envMem := 0.60
	groups := []GroupThreshold{
		{Scope: "role", Label: "web", ThresholdOverride: ThresholdOverride{MemoryEnabled: &on, MemoryThreshold: &roleMem}},
		{Scope: "env", Label: "prod", ThresholdOverride: ThresholdOverride{MemoryEnabled: &on, MemoryThreshold: &envMem}},
	}
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: true, Groups: groups}
	h := Host{ID: 1, Environment: "prod", Role: "web"}
	eff := r.Effective(h)
	if eff.Settings.MemoryThreshold != 0.75 || eff.MemorySource.Level != "role" || eff.MemorySource.Label != "web" {
		t.Fatalf("role must beat env: %v %+v", eff.Settings.MemoryThreshold, eff.MemorySource)
	}
}

func TestResolverEnvOnlySource(t *testing.T) {
	on := true
	envDisk := 0.65
	groups := []GroupThreshold{
		{Scope: "env", Label: "prod", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &envDisk}},
	}
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: true, Groups: groups}
	h := Host{ID: 1, Environment: "prod", Role: "db"}
	eff := r.Effective(h)
	if eff.Settings.DiskThreshold != 0.65 || eff.DiskSource.Level != "env" || eff.DiskSource.Label != "prod" {
		t.Fatalf("env-only source: %v %+v", eff.Settings.DiskThreshold, eff.DiskSource)
	}
}

func TestResolverProjectMissingFallsToDefault(t *testing.T) {
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: false}
	h := Host{ID: 1, Environment: "prod", Role: "web"}
	eff := r.Effective(h)
	if eff.LoadSource.Level != "default" {
		t.Fatalf("expected default, got: %+v", eff.LoadSource)
	}
}

func TestResolverEmptyLabelSkipsGroup(t *testing.T) {
	on := true
	groupDisk := 0.55
	groups := []GroupThreshold{
		{Scope: "role", Label: "", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &groupDisk}},
	}
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: true, Groups: groups}
	h := Host{ID: 1, Environment: "", Role: ""}
	eff := r.Effective(h)
	if eff.DiskSource.Level != "project" {
		t.Fatalf("empty label must not match group, got: %+v", eff.DiskSource)
	}
}
