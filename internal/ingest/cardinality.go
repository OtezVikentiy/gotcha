package ingest

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Схлопываем вместо отбрасывания: суммарные throughput и латентность по
// проекту остаются верными, пропадает только разбивка по хвосту.
const CardinalityOverflow = "<cardinality-limit>"

// Поля входят в ключ сортировки/GROUP BY представлений ClickHouse — новое
// значение создаёт несхлопываемую строку агрегата.
const (
	FieldTransaction = "transaction"
	FieldEnvironment = "environment"
	FieldMetricName  = "metric_name"
	FieldService     = "service"
	FieldOp          = "op"
	// Единственное поле сверх ключа сортировки metric_points, где применим этот
	// гард — остальные атрибуты точки под ограничение не попадают вовсе.
	FieldHost = "host"
)

const (
	defaultCardinalityLimit  = 10000
	defaultCardinalityWindow = time.Hour
	maxCardinalitySamples    = 5
	maxCardinalityProjects   = 2000
	maxCardinalityFields     = 200
	defaultMaxTrackedValues  = 1 << 20
)

type fieldState struct {
	seen      map[string]struct{}
	collapsed int64
	samples   []string
}

// В памяти процесса намеренно: защита приёма обязана отвечать за наносекунды,
// не ходить в БД. Расхождение между репликами приемлемо — потолок нестрогий.
type CardinalityGuard struct {
	mu         sync.Mutex
	limit      int
	maxTracked int // 0 означает дефолт (defaultMaxTrackedValues)
	tracked    int
	window     time.Duration
	now        func() time.Time
	projects   map[int64]*projectCardinality
	// Атомарный и монотонный (не убывает): per-project счётчики пропадают при
	// ролловере/вытеснении, а /metrics не должен брать g.mu на каждый скрап.
	collapsedTotal atomic.Int64
}

type projectCardinality struct {
	windowStart time.Time
	fields      map[string]*fieldState
}

// limit<=0 отключает ограничение целиком — осознанный выбор оператора для
// инсталляции с доверенными отправителями.
func NewCardinalityGuard(limit int, window time.Duration) *CardinalityGuard {
	if window <= 0 {
		window = defaultCardinalityWindow
	}
	return &CardinalityGuard{
		limit:      limit,
		maxTracked: defaultMaxTrackedValues,
		window:     window,
		now:        time.Now,
		projects:   make(map[int64]*projectCardinality),
	}
}

// Пустое значение не считается: отсутствие имени — не новое имя.
func (g *CardinalityGuard) Value(projectID int64, field, value string) string {
	if g == nil || g.limit <= 0 || value == "" || value == CardinalityOverflow {
		return value
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.projects[projectID]
	if !ok {
		if len(g.projects) >= maxCardinalityProjects {
			g.evictLocked(now)
		}
		p = &projectCardinality{windowStart: now, fields: map[string]*fieldState{}}
		g.projects[projectID] = p
	}
	// Без сброса проект, починивший имена, остался бы схлопнутым до перезапуска.
	if now.Sub(p.windowStart) >= g.window {
		g.tracked -= p.trackedValues()
		p.windowStart = now
		p.fields = map[string]*fieldState{}
	}

	f, ok := p.fields[field]
	if !ok {
		// Имя поля тоже задаёт отправитель: без этой ветки «поле на событие»
		// обходило бы ограничение целиком.
		if len(p.fields) >= maxCardinalityFields {
			return CardinalityOverflow
		}
		f = &fieldState{seen: make(map[string]struct{}, 64)}
		p.fields[field] = f
	}
	if _, known := f.seen[value]; known {
		return value
	}
	if len(f.seen) < g.limit && g.hasBudgetLocked(now) {
		f.seen[value] = struct{}{}
		g.tracked++
		return value
	}

	f.collapsed++
	g.collapsedTotal.Add(1)
	if len(f.samples) < maxCardinalitySamples {
		f.samples = append(f.samples, value)
	}
	return CardinalityOverflow
}

// Полный сброс снял бы ограничение ровно с тех проектов, которые в него упёрлись.
func (g *CardinalityGuard) evictLocked(now time.Time) {
	for id, p := range g.projects {
		if now.Sub(p.windowStart) >= g.window {
			g.tracked -= p.trackedValues()
			delete(g.projects, id)
		}
	}
	if len(g.projects) < maxCardinalityProjects {
		return
	}
	drop := len(g.projects) / 10
	if drop == 0 {
		drop = 1
	}
	for id, p := range g.projects {
		if drop == 0 {
			break
		}
		g.tracked -= p.trackedValues()
		delete(g.projects, id)
		drop--
	}
}

// При исчерпании бюджета сначала выбрасываются проекты с истёкшим окном; если и
// это не помогло, новые значения просто перестают запоминаться и схлопываются.
func (g *CardinalityGuard) hasBudgetLocked(now time.Time) bool {
	max := g.maxTracked
	if max <= 0 {
		max = defaultMaxTrackedValues
	}
	if g.tracked < max {
		return true
	}
	for id, p := range g.projects {
		if now.Sub(p.windowStart) >= g.window {
			g.tracked -= p.trackedValues()
			delete(g.projects, id)
		}
	}
	return g.tracked < max
}

func (g *CardinalityGuard) TrackedValues() int64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return int64(g.tracked)
}

func (p *projectCardinality) trackedValues() int {
	var n int
	for _, f := range p.fields {
		n += len(f.seen)
	}
	return n
}

type FieldReport struct {
	Field       string
	Distinct    int
	Limit       int
	Collapsed   int64
	Samples     []string
	WindowStart time.Time
}

func (g *CardinalityGuard) Report(projectID int64) []FieldReport {
	if g == nil || g.limit <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.projects[projectID]
	if !ok || g.now().Sub(p.windowStart) >= g.window {
		return nil
	}
	var out []FieldReport
	for name, f := range p.fields {
		if f.collapsed == 0 {
			continue
		}
		out = append(out, FieldReport{
			Field:       name,
			Distinct:    len(f.seen),
			Limit:       g.limit,
			Collapsed:   f.collapsed,
			Samples:     append([]string(nil), f.samples...),
			WindowStart: p.windowStart,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func (g *CardinalityGuard) CollapsedTotal() int64 {
	if g == nil {
		return 0
	}
	return g.collapsedTotal.Load()
}

func FieldLabel(field string) string {
	switch field {
	case FieldTransaction:
		return "transaction name"
	case FieldEnvironment:
		return "environment"
	case FieldMetricName:
		return "metric name"
	case FieldService:
		return "service"
	case FieldOp:
		return "span operation"
	case FieldHost:
		return "host"
	default:
		return strings.ReplaceAll(field, "_", " ")
	}
}
