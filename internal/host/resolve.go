package host

import "time"

// Level — уровень каскада, с которого взято эффективное значение вида
// порога (disk/memory/load/silent).
type Level string

const (
	LevelHost    Level = "host"
	LevelRole    Level = "role"
	LevelEnv     Level = "env"
	LevelProject Level = "project"
	LevelDefault Level = "default"
)

// ThresholdSource — откуда резолвер взял эффективное значение вида порога:
// уровень каскада и, для role/env, метка (имя роли/окружения), давшая
// значение. Для host/project/default Label пуст — уровень однозначен и без
// метки.
type ThresholdSource struct {
	Level Level
	Label string
}

// EffectiveSettings — результат ThresholdResolver.Effective для одного
// хоста: плоский Settings (M1) для оценщика/Notifier, которым безразлично
// происхождение каждого порога, и источник по каждому виду — для UI
// («почему тут именно это число»).
type EffectiveSettings struct {
	Settings Settings

	DiskSource   ThresholdSource
	MemorySource ThresholdSource
	LoadSource   ThresholdSource
	SilentSource ThresholdSource
}

// ThresholdResolver — чистый (без БД) резолвер каскада порогов хоста:
// host-override → role-group → env-group → project → default, отдельно по
// каждому виду и раздельно enabled/value (M3, см. Effective).
type ThresholdResolver struct {
	Project       Settings
	ProjectExists bool
	Groups        []GroupThreshold
	Overrides     map[int64]ThresholdOverride
}

// group ищет групповой порог (scope,label) в r.Groups линейным поиском —
// список короткий, отдельный индекс не нужен. Пустая метка (h.Role=="" /
// h.Environment=="") никогда не матчит: соответствующий уровень каскада для
// такого хоста молча пропускается (ok=false), а не совпадает с групповым
// порогом, у которого тоже могла бы оказаться пустая метка.
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

// levelCandidate — один уровень каскада для резолвинга ОДНОГО вида порога:
// значения enabled/value на этом уровне (nil = не задано здесь) и Level/
// Label, которые попадут в ThresholdSource, если этот уровень станет
// источником enabled.
type levelCandidate[T any] struct {
	level   Level
	label   string
	enabled *bool
	value   *T
}

// levelCandidates собирает кандидатов каскада для одного вида порога в
// порядке [host, role, env] (роль ПЕРЕД env — см. бриф). get достаёт пару
// (enabled,value) нужного вида из ThresholdOverride; roleOv/envOv уже пустые
// ThresholdOverride{}, если group() не нашла соответствующий уровень (в т.ч.
// из-за пустой метки) — тогда оба указателя nil и кандидат ни на что не
// влияет, отдельная ветка на этот случай не нужна.
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

// resolveKind — каскад одного вида порога (M3): enabled и value резолвятся
// НЕЗАВИСИМО, каждый первым non-nil значением по кандидатам в их порядке
// (candidates уже [host,role,env]), а после них — проектным
// enabled/value (Project.value всегда задан, поэтому цепочка value
// гарантированно на чём-то остановится). Источник в ThresholdSource — это
// источник ENABLED, а не value: выключенный вид показывает «выключено» по
// enabled-источнику, а число — эффективное унаследованное значение (может
// прийти с более глубокого уровня, чем enabled).
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

// Effective резолвит эффективные пороги хоста h по каскаду
// host-override → role-group → env-group → project → default, отдельно по
// каждому из 4 видов (disk/memory/load/silent).
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
