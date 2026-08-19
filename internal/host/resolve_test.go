package host

import "testing"

func TestResolverCascade(t *testing.T) {
	proj := DefaultSettings()
	proj.DiskThreshold = 0.85 // проектный диск 85%
	on := true
	off := false
	rDisk := 0.70
	rLoad := 4.0
	groups := []GroupThreshold{
		{Scope: "role", Label: "web", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &rDisk}}, // роль web: диск 70%
		{Scope: "env", Label: "prod", ThresholdOverride: ThresholdOverride{LoadEnabled: &on, LoadThreshold: &rLoad}}, // env prod: load 4.0
	}
	hLoad := 6.0
	ovr := map[int64]ThresholdOverride{
		1: {LoadEnabled: &on, LoadThreshold: &hLoad, MemoryEnabled: &off}, // хост: load 6.0, memory ВЫКЛ
	}
	r := ThresholdResolver{Project: proj, ProjectExists: true, Groups: groups, Overrides: ovr}
	h := Host{ID: 1, Environment: "prod", Role: "web"}
	eff := r.Effective(h)
	// disk: роль web (host не задал) → 70%, источник role/web.
	if eff.Settings.DiskThreshold != 0.70 || eff.DiskSource.Level != "role" || eff.DiskSource.Label != "web" {
		t.Fatalf("disk: %v %+v", eff.Settings.DiskThreshold, eff.DiskSource)
	}
	// load: host > env — host 6.0 выигрывает (роль>env не важно, host выше).
	if eff.Settings.LoadThreshold != 6.0 || eff.LoadSource.Level != "host" {
		t.Fatalf("load: %v %+v", eff.Settings.LoadThreshold, eff.LoadSource)
	}
	// memory: host выключил → Enabled=false, источник host; значение (M3) наследуется от проекта 90%.
	if eff.Settings.MemoryEnabled != false || eff.MemorySource.Level != "host" || eff.Settings.MemoryThreshold != 0.90 {
		t.Fatalf("memory: %+v", eff.Settings)
	}
	// silent: никто не задал → проект/дефолт, источник project (ProjectExists=true).
	if eff.SilentSource.Level != "project" {
		t.Fatalf("silent src: %+v", eff.SilentSource)
	}
}
