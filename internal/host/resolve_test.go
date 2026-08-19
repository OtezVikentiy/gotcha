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

// TestResolverRoleBeatsEnv — роль ВЫШЕ env, когда оба уровня задают
// enabled+value ОДНОГО вида (без участия host): порядок кандидатов должен
// быть строго [host,role,env], а не [host,env,role] — иначе тест остаётся
// зелёным даже при перепутанном порядке.
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

// TestResolverEnvOnlySource — env как ЕДИНСТВЕННЫЙ источник (host и role его
// не касаются): Source.Level=="env" с меткой окружения.
func TestResolverEnvOnlySource(t *testing.T) {
	on := true
	envDisk := 0.65
	groups := []GroupThreshold{
		{Scope: "env", Label: "prod", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &envDisk}},
	}
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: true, Groups: groups}
	h := Host{ID: 1, Environment: "prod", Role: "db"} // роль db — группы для неё нет
	eff := r.Effective(h)
	if eff.Settings.DiskThreshold != 0.65 || eff.DiskSource.Level != "env" || eff.DiskSource.Label != "prod" {
		t.Fatalf("env-only source: %v %+v", eff.Settings.DiskThreshold, eff.DiskSource)
	}
}

// TestResolverProjectMissingFallsToDefault — ProjectExists=false: вид, не
// тронутый ни host, ни группами, обязан упасть на "default", а не "project"
// (проектной строки в БД ещё нет — Project содержит DefaultSettings()).
func TestResolverProjectMissingFallsToDefault(t *testing.T) {
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: false}
	h := Host{ID: 1, Environment: "prod", Role: "web"}
	eff := r.Effective(h)
	if eff.LoadSource.Level != "default" {
		t.Fatalf("expected default, got: %+v", eff.LoadSource)
	}
}

// TestResolverEmptyLabelSkipsGroup — пустая метка хоста (Role=="" здесь)
// НЕ матчит групповой порог, даже если в Groups лежит запись с такой же
// пустой Label: групповой уровень пропускается целиком, каскад падает на
// project/default.
func TestResolverEmptyLabelSkipsGroup(t *testing.T) {
	on := true
	groupDisk := 0.55
	groups := []GroupThreshold{
		{Scope: "role", Label: "", ThresholdOverride: ThresholdOverride{DiskEnabled: &on, DiskThreshold: &groupDisk}},
	}
	r := ThresholdResolver{Project: DefaultSettings(), ProjectExists: true, Groups: groups}
	h := Host{ID: 1, Environment: "", Role: ""} // метки не пришли ни разу
	eff := r.Effective(h)
	if eff.DiskSource.Level != "project" {
		t.Fatalf("empty label must not match group, got: %+v", eff.DiskSource)
	}
}
