package host

import "time"

type Level string

const (
	LevelHost    Level = "host"
	LevelRole    Level = "role"
	LevelEnv     Level = "env"
	LevelProject Level = "project"
	LevelDefault Level = "default"
)

// Label пуст для host/project/default — уровень однозначен и без метки.
type ThresholdSource struct {
	Level Level
	Label string
}

type EffectiveSettings struct {
	Settings Settings

	DiskSource   ThresholdSource
	MemorySource ThresholdSource
	LoadSource   ThresholdSource
	SilentSource ThresholdSource
}

// Каскад: host-override → role-group → env-group → project → default, раздельно по enabled/value.
type ThresholdResolver struct {
	Project       Settings
	ProjectExists bool
	Groups        []GroupThreshold
	Overrides     map[int64]ThresholdOverride
}

// Пустая метка (h.Role/h.Environment == "") никогда не матчит групповой порог —
// уровень каскада для такого хоста молча пропускается, а не совпадает с пустой меткой группы.
func (r ThresholdResolver) group(scope, label string) (ThresholdOverride, bool) {
	if label == "" {
		return ThresholdOverride{}, false
	}
	for _, g := range r.Groups {
		if g.Scope == scope && g.Label == label {
			return g.ThresholdOverride, true
		}
	}
	return ThresholdOverride{}, false
}

type levelCandidate[T any] struct {
	level   Level
	label   string
	enabled *bool
	value   *T
}

// Порядок — [host, role, env]: роль приоритетнее env.
func levelCandidates[T any](hostOv, roleOv, envOv ThresholdOverride, h Host, get func(ThresholdOverride) (*bool, *T)) []levelCandidate[T] {
	hostE, hostV := get(hostOv)
	roleE, roleV := get(roleOv)
	envE, envV := get(envOv)
	return []levelCandidate[T]{
		{level: LevelHost, enabled: hostE, value: hostV},
		{level: LevelRole, label: h.Role, enabled: roleE, value: roleV},
		{level: LevelEnv, label: h.Environment, enabled: envE, value: envV},
	}
}

// enabled и value резолвятся НЕЗАВИСИМО первым non-nil кандидатом; Source — источник ENABLED,
// не value: число может унаследоваться с более глубокого уровня, чем флаг включённости.
func resolveKind[T any](candidates []levelCandidate[T], projectEnabled bool, projectValue T, projectLevel Level) (enabled bool, value T, src ThresholdSource) {
	enabled = projectEnabled
	value = projectValue
	src = ThresholdSource{Level: projectLevel}

	for _, c := range candidates {
		if c.enabled != nil {
			enabled = *c.enabled
			src = ThresholdSource{Level: c.level, Label: c.label}
			break
		}
	}
	for _, c := range candidates {
		if c.value != nil {
			value = *c.value
			break
		}
	}
	return enabled, value, src
}

func (r ThresholdResolver) Effective(h Host) EffectiveSettings {
	hostOv := r.Overrides[h.ID]
	roleOv, _ := r.group("role", h.Role)
	envOv, _ := r.group("env", h.Environment)

	projectLevel := LevelDefault
	if r.ProjectExists {
		projectLevel = LevelProject
	}

	var out EffectiveSettings

	out.Settings.DiskEnabled, out.Settings.DiskThreshold, out.DiskSource = resolveKind(
		levelCandidates(hostOv, roleOv, envOv, h,
			func(o ThresholdOverride) (*bool, *float64) { return o.DiskEnabled, o.DiskThreshold }),
		r.Project.DiskEnabled, r.Project.DiskThreshold, projectLevel)

	out.Settings.MemoryEnabled, out.Settings.MemoryThreshold, out.MemorySource = resolveKind(
		levelCandidates(hostOv, roleOv, envOv, h,
			func(o ThresholdOverride) (*bool, *float64) { return o.MemoryEnabled, o.MemoryThreshold }),
		r.Project.MemoryEnabled, r.Project.MemoryThreshold, projectLevel)

	out.Settings.LoadEnabled, out.Settings.LoadThreshold, out.LoadSource = resolveKind(
		levelCandidates(hostOv, roleOv, envOv, h,
			func(o ThresholdOverride) (*bool, *float64) { return o.LoadEnabled, o.LoadThreshold }),
		r.Project.LoadEnabled, r.Project.LoadThreshold, projectLevel)

	out.Settings.SilentEnabled, out.Settings.SilentAfter, out.SilentSource = resolveKind(
		levelCandidates(hostOv, roleOv, envOv, h,
			func(o ThresholdOverride) (*bool, *time.Duration) { return o.SilentEnabled, o.SilentAfter }),
		r.Project.SilentEnabled, r.Project.SilentAfter, projectLevel)

	return out
}
