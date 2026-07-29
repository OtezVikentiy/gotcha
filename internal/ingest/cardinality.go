package ingest

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// CardinalityOverflow — значение, которым заменяются имена сверх потолка.
//
// Схлопываем, а не отбрасываем: отбросив строку целиком, мы потеряли бы и сам
// факт нагрузки, а так суммарные throughput и латентность по проекту остаются
// верными — пропадает только разбивка по хвосту.
const CardinalityOverflow = "<cardinality-limit>"

// Поля, кардинальность которых ограничивается. Значения этих полей приходят от
// клиента и по природе открыты, а в ClickHouse они стоят в ключах сортировки и
// в GROUP BY материализованных представлений: каждое новое значение создаёт
// новую строку агрегата с состояниями квантилей, которая не схлопнётся ни с
// чем. Один идентификатор, случайно попавший в имя, превращает десяток
// эндпойнтов в сотни тысяч.
const (
	FieldTransaction = "transaction"
	FieldEnvironment = "environment"
	FieldMetricName  = "metric_name"
	FieldService     = "service"
	FieldOp          = "op"
)

const (
	// defaultCardinalityLimit — сколько РАЗЛИЧНЫХ значений поля допускается на
	// проект в пределах окна. Щедро: у честного продукта эндпойнтов десятки или
	// сотни, тысячи — уже признак переменной в имени.
	defaultCardinalityLimit = 10000
	// defaultCardinalityWindow — окно, после которого набор различённых значений
	// начинается заново. Без него однажды переполнившийся проект остался бы
	// схлопнутым навсегда, даже починив имена.
	defaultCardinalityWindow = time.Hour
	// maxCardinalitySamples — сколько схлопнутых значений запоминать для показа.
	// Примеры — самое ценное в диагностике: три строки подряд с разными числами
	// объясняют причину быстрее любого счётчика.
	maxCardinalitySamples = 5
	// maxCardinalityProjects — сколько проектов отслеживать одновременно.
	// Как у KeyCache и рейт-лимитера: без потолка карта растёт неограниченно.
	maxCardinalityProjects = 2000
)

// fieldState — состояние одного поля одного проекта.
type fieldState struct {
	seen      map[string]struct{}
	collapsed int64
	samples   []string
}

// CardinalityGuard ограничивает число различных значений полей на проект.
//
// Живёт в памяти процесса намеренно: это защита приёма, она обязана отвечать за
// наносекунды и не ходить в БД на каждую строку. Расхождение между репликами
// приемлемо — потолок нестрогий по построению, его задача срезать взрыв, а не
// посчитать до единицы.
type CardinalityGuard struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	now      func() time.Time
	projects map[int64]*projectCardinality
}

type projectCardinality struct {
	windowStart time.Time
	fields      map[string]*fieldState
}

// NewCardinalityGuard создаёт ограничитель. limit<=0 ВЫКЛЮЧАЕТ ограничение —
// осознанный выбор оператора для инсталляции с доверенными отправителями.
func NewCardinalityGuard(limit int, window time.Duration) *CardinalityGuard {
	if window <= 0 {
		window = defaultCardinalityWindow
	}
	return &CardinalityGuard{
		limit:    limit,
		window:   window,
		now:      time.Now,
		projects: make(map[int64]*projectCardinality),
	}
}

// Value возвращает значение, которое следует записать: само value, если проект
// ещё не упёрся в потолок по этому полю, иначе CardinalityOverflow.
//
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
	// Окно истекло — начинаем набор заново: проект, починивший имена, обязан
	// вернуться к нормальной работе без перезапуска инстанса.
	if now.Sub(p.windowStart) >= g.window {
		p.windowStart = now
		p.fields = map[string]*fieldState{}
	}

	f, ok := p.fields[field]
	if !ok {
		f = &fieldState{seen: make(map[string]struct{}, 64)}
		p.fields[field] = f
	}
	if _, known := f.seen[value]; known {
		return value
	}
	if len(f.seen) < g.limit {
		f.seen[value] = struct{}{}
		return value
	}

	f.collapsed++
	if len(f.samples) < maxCardinalitySamples {
		f.samples = append(f.samples, value)
	}
	return CardinalityOverflow
}

// evictLocked освобождает место, выбрасывая проекты с истёкшим окном, а если
// таких нет — десятую часть произвольных. Полный сброс здесь был бы тем же
// дефектом, что уже чинили в рейт-лимитере: он снял бы ограничение ровно с тех
// проектов, которые в него упёрлись.
func (g *CardinalityGuard) evictLocked(now time.Time) {
	for id, p := range g.projects {
		if now.Sub(p.windowStart) >= g.window {
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
	for id := range g.projects {
		if drop == 0 {
			break
		}
		delete(g.projects, id)
		drop--
	}
}

// FieldReport — состояние одного поля проекта для диагностики.
type FieldReport struct {
	Field string
	// Distinct — сколько различных значений набрано в текущем окне.
	Distinct int
	// Limit — потолок.
	Limit int
	// Collapsed — сколько значений схлопнуто в текущем окне.
	Collapsed int64
	// Samples — примеры схлопнутых значений. Ради них отчёт и существует:
	// три имени подряд с разными числами объясняют причину мгновенно.
	Samples []string
	// WindowStart — начало текущего окна.
	WindowStart time.Time
}

// Report отдаёт поля проекта, которые упёрлись в потолок. Пусто — всё в норме.
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

// CollapsedTotal — сколько значений схлопнуто по всем проектам за жизнь
// процесса. Для /metrics: оператор обязан видеть, что где-то режется хвост.
func (g *CardinalityGuard) CollapsedTotal() int64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var n int64
	for _, p := range g.projects {
		for _, f := range p.fields {
			n += f.collapsed
		}
	}
	return n
}

// FieldLabel — человекочитаемое имя поля для интерфейса и документации.
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
	default:
		return strings.ReplaceAll(field, "_", " ")
	}
}
