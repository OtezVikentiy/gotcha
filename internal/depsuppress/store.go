// Package depsuppress хранит рёбра зависимостей между узлами проекта
// (хост/монитор/группа хостов по метке env-role) и валидирует их перед
// сохранением. Рёбра используются волной B5 для подавления шторма алертов:
// инцидент дочернего узла подавляется, пока у родителя открыт свой.
package depsuppress

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Edge — ребро зависимости alert_dependencies. Родитель — ровно один из
// ParentHostID/ParentMonitorID; ребёнок — ровно один способ: ChildHostID,
// ChildMonitorID, либо пара ChildLabelScope/ChildLabelValue (селектор по
// метке "env" или "role").
type Edge struct {
	ID                               int64
	ProjectID                        int64
	ParentHostID, ParentMonitorID    *int64
	ChildHostID, ChildMonitorID      *int64
	ChildLabelScope, ChildLabelValue *string
}

// Ошибки валидации Create. Обёрнуты через fmt.Errorf("%w: ...") — errors.Is
// работает через цепочку.
var (
	// ErrInvalidEdge — неверная форма ребра (не ровно один родитель/способ
	// ребёнка, либо scope не из {env,role}).
	ErrInvalidEdge = errors.New("depsuppress: invalid edge")
	// ErrForeignNode — узел (host/monitor) не принадлежит project_id ребра.
	ErrForeignNode = errors.New("depsuppress: node belongs to another project")
	// ErrSelfLoop — родитель и ребёнок указывают на один и тот же узел.
	ErrSelfLoop = errors.New("depsuppress: self loop")
	// ErrSelfMatch — ребёнок-label-селектор матчит собственную метку
	// родителя-хоста.
	ErrSelfMatch = errors.New("depsuppress: label selector matches parent itself")
	// ErrDuplicate — точно такое же ребро уже существует в проекте.
	ErrDuplicate = errors.New("depsuppress: duplicate edge")
	// ErrCycle — ребро замыкает цикл среди явных узлов графа зависимостей.
	ErrCycle = errors.New("depsuppress: cycle among explicit nodes")
	// ErrNotFound — ребро с таким id в проекте не существует. Update
	// намеренно не различает «нет вовсе» и «принадлежит другому проекту»:
	// вызывающий web-слой отвечает единообразным 404, не раскрывая
	// существование чужой строки (тот же принцип, что uniform 404 кабинета).
	ErrNotFound = errors.New("depsuppress: edge not found")
)

// Store — CRUD рёбер зависимостей поверх таблицы alert_dependencies.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore создаёт Store поверх пула соединений PostgreSQL.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// List возвращает все рёбра проекта, отсортированные по id (стабильный
// порядок для UI и тестов).
func (s *Store) List(ctx context.Context, projectID int64) ([]Edge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, parent_host_id, parent_monitor_id,
		       child_host_id, child_monitor_id, child_label_scope, child_label_value
		FROM alert_dependencies
		WHERE project_id = $1
		ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("depsuppress: list edges: %w", err)
	}
	defer rows.Close()

	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.ParentHostID, &e.ParentMonitorID,
			&e.ChildHostID, &e.ChildMonitorID, &e.ChildLabelScope, &e.ChildLabelValue,
		); err != nil {
			return nil, fmt.Errorf("depsuppress: scan edge row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("depsuppress: list edges: %w", err)
	}
	return out, nil
}

// Create валидирует ребро (форма → принадлежность проекту → self-loop →
// self-match label → дубликат → цикл) и вставляет его. Все проверки и вставка
// идут в одной транзакции, но под READ COMMITTED без FOR UPDATE/advisory-lock/
// UNIQUE две параллельные Create НЕ видят вставки друг друга — конкурентная
// вставка дубля или замыкающего цикл ребра при точной гонке возможна. Это
// benign: дубликаты дедуплицируются резолвером (matchingParents идёт по
// множеству, повтор ребра ничего не меняет), а цикл среди рёбер безопасен —
// обход ParentDown цикло-устойчив (visited-множество, обход до down-корня),
// паники/зависания нет. Гонка настолько редка (ручное редактирование графа
// оператором), что цена строгой сериализации не оправдана.
func (s *Store) Create(ctx context.Context, e Edge) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("depsuppress: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := validateShape(e); err != nil {
		return 0, err
	}
	if err := checkNodesBelongToProject(ctx, tx, e); err != nil {
		return 0, err
	}
	if err := checkSelfLoop(e); err != nil {
		return 0, err
	}
	if err := checkSelfMatch(ctx, tx, e); err != nil {
		return 0, err
	}
	if err := checkDuplicate(ctx, tx, e, 0); err != nil {
		return 0, err
	}
	if err := checkCycle(ctx, tx, e, 0); err != nil {
		return 0, err
	}

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO alert_dependencies (
			project_id, parent_host_id, parent_monitor_id,
			child_host_id, child_monitor_id, child_label_scope, child_label_value
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		e.ProjectID, e.ParentHostID, e.ParentMonitorID,
		e.ChildHostID, e.ChildMonitorID, e.ChildLabelScope, e.ChildLabelValue,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("depsuppress: insert edge: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("depsuppress: commit: %w", err)
	}
	return id, nil
}

// Update заменяет содержимое существующего ребра e.ID (скоуп — e.ProjectID)
// новой формой, сохраняя id: якоря модалок правки и адреса POST остаются
// стабильными между рендерами. Цепочка валидации — та же, что у Create, но
// checkDuplicate/checkCycle исключают само редактируемое ребро: сохранение
// без изменений не должно падать дубликатом самого себя, а разворот A→B в
// B→A — ловить «цикл» с собственной старой версией. Несуществующее ребро или
// ребро чужого проекта — ErrNotFound (см. докблок ошибки). SELECT ... FOR
// UPDATE держит строку до конца транзакции — конкурентная правка того же
// ребра сериализуется; гонки с параллельным Create других рёбер остаются
// теми же benign-гонками, что описаны у Create.
//
// На уже открытые подавленные инциденты правка не влияет: флаг
// suppressed_by_dep одноразовый (его ставят Suppressor.MarkSuppressed и
// uptime.Service.MarkSuppressedByDep, обратного писателя нет) — новое ребро
// увидят только будущие решения о подавлении, через перезагрузку снимка
// Suppressor не позже cacheTTL.
func (s *Store) Update(ctx context.Context, e Edge) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("depsuppress: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := validateShape(e); err != nil {
		return err
	}
	var lockedID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM alert_dependencies WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		e.ID, e.ProjectID,
	).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: edge %d in project %d", ErrNotFound, e.ID, e.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("depsuppress: lock edge %d: %w", e.ID, err)
	}
	if err := checkNodesBelongToProject(ctx, tx, e); err != nil {
		return err
	}
	if err := checkSelfLoop(e); err != nil {
		return err
	}
	if err := checkSelfMatch(ctx, tx, e); err != nil {
		return err
	}
	if err := checkDuplicate(ctx, tx, e, e.ID); err != nil {
		return err
	}
	if err := checkCycle(ctx, tx, e, e.ID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE alert_dependencies SET
			parent_host_id = $1, parent_monitor_id = $2,
			child_host_id = $3, child_monitor_id = $4,
			child_label_scope = $5, child_label_value = $6
		WHERE id = $7 AND project_id = $8`,
		e.ParentHostID, e.ParentMonitorID,
		e.ChildHostID, e.ChildMonitorID, e.ChildLabelScope, e.ChildLabelValue,
		e.ID, e.ProjectID,
	); err != nil {
		return fmt.Errorf("depsuppress: update edge %d: %w", e.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("depsuppress: commit: %w", err)
	}
	return nil
}

// Delete удаляет ребро проекта. Отсутствие строки — не ошибка (идемпотентно,
// как принято в остальных стораджах продукта).
func (s *Store) Delete(ctx context.Context, projectID, id int64) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM alert_dependencies WHERE project_id = $1 AND id = $2`, projectID, id)
	if err != nil {
		return fmt.Errorf("depsuppress: delete edge: %w", err)
	}
	return nil
}

// validateShape проверяет форму ребра: ровно один родитель, ровно один
// способ задать ребёнка, scope (если задан) — из допустимого набора.
func validateShape(e Edge) error {
	parents := 0
	if e.ParentHostID != nil {
		parents++
	}
	if e.ParentMonitorID != nil {
		parents++
	}
	if parents != 1 {
		return fmt.Errorf("%w: expected exactly one parent, got %d", ErrInvalidEdge, parents)
	}

	hasLabel := e.ChildLabelScope != nil || e.ChildLabelValue != nil
	if hasLabel && (e.ChildLabelScope == nil || e.ChildLabelValue == nil) {
		return fmt.Errorf("%w: child label scope/value must be set together", ErrInvalidEdge)
	}

	children := 0
	if e.ChildHostID != nil {
		children++
	}
	if e.ChildMonitorID != nil {
		children++
	}
	if hasLabel {
		children++
	}
	if children != 1 {
		return fmt.Errorf("%w: expected exactly one way to specify child, got %d", ErrInvalidEdge, children)
	}

	if hasLabel && *e.ChildLabelScope != "env" && *e.ChildLabelScope != "role" {
		return fmt.Errorf("%w: child label scope must be env or role, got %q", ErrInvalidEdge, *e.ChildLabelScope)
	}
	return nil
}

// checkNodesBelongToProject проверяет, что каждый указанный узел (host или
// monitor) принадлежит project_id ребра. Для label-рёбер проверяется только
// родитель — ребёнок не ссылается на конкретный узел.
func checkNodesBelongToProject(ctx context.Context, tx pgx.Tx, e Edge) error {
	if e.ParentHostID != nil {
		if err := checkOwnership(ctx, tx, "hosts", *e.ParentHostID, e.ProjectID); err != nil {
			return err
		}
	}
	if e.ParentMonitorID != nil {
		if err := checkOwnership(ctx, tx, "monitors", *e.ParentMonitorID, e.ProjectID); err != nil {
			return err
		}
	}
	if e.ChildHostID != nil {
		if err := checkOwnership(ctx, tx, "hosts", *e.ChildHostID, e.ProjectID); err != nil {
			return err
		}
	}
	if e.ChildMonitorID != nil {
		if err := checkOwnership(ctx, tx, "monitors", *e.ChildMonitorID, e.ProjectID); err != nil {
			return err
		}
	}
	return nil
}

func checkOwnership(ctx context.Context, tx pgx.Tx, table string, id, projectID int64) error {
	var exists bool
	// table — константа из вызывающего кода (не пользовательский ввод), safe to interpolate.
	q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1 AND project_id = $2)`, table)
	if err := tx.QueryRow(ctx, q, id, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("depsuppress: check ownership of %s %d: %w", table, id, err)
	}
	if !exists {
		return fmt.Errorf("%w: %s %d not in project %d", ErrForeignNode, table, id, projectID)
	}
	return nil
}

// checkSelfLoop отвергает ребро, где родитель и ребёнок — один и тот же узел
// (host==host или monitor==monitor).
func checkSelfLoop(e Edge) error {
	if e.ParentHostID != nil && e.ChildHostID != nil && *e.ParentHostID == *e.ChildHostID {
		return fmt.Errorf("%w: host %d references itself", ErrSelfLoop, *e.ParentHostID)
	}
	if e.ParentMonitorID != nil && e.ChildMonitorID != nil && *e.ParentMonitorID == *e.ChildMonitorID {
		return fmt.Errorf("%w: monitor %d references itself", ErrSelfLoop, *e.ParentMonitorID)
	}
	return nil
}

// checkSelfMatch отвергает ребро, где ребёнок — label-селектор, родитель —
// host, и этот host сам матчит собственный селектор (например: parent host
// role='web', child selector scope=role value='web').
func checkSelfMatch(ctx context.Context, tx pgx.Tx, e Edge) error {
	if e.ChildLabelScope == nil || e.ChildLabelValue == nil || e.ParentHostID == nil {
		return nil
	}
	var environment, role string
	err := tx.QueryRow(ctx, `SELECT environment, role FROM hosts WHERE id = $1`, *e.ParentHostID).
		Scan(&environment, &role)
	if err != nil {
		return fmt.Errorf("depsuppress: load parent host labels: %w", err)
	}
	var parentValue string
	switch *e.ChildLabelScope {
	case "env":
		parentValue = environment
	case "role":
		parentValue = role
	}
	if parentValue == *e.ChildLabelValue {
		return fmt.Errorf("%w: host %d already matches %s=%s", ErrSelfMatch, *e.ParentHostID, *e.ChildLabelScope, *e.ChildLabelValue)
	}
	return nil
}

// checkDuplicate отвергает ребро, точно совпадающее (NULL-safe) с уже
// существующим рёбром проекта. excludeID — id самого редактируемого ребра
// при Update (его собственная строка — не дубликат себя); 0 при Create —
// id из bigserial начинаются с 1, ноль не исключает ничего.
func checkDuplicate(ctx context.Context, tx pgx.Tx, e Edge, excludeID int64) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM alert_dependencies
			WHERE project_id = $1
			  AND id <> $8
			  AND parent_host_id IS NOT DISTINCT FROM $2
			  AND parent_monitor_id IS NOT DISTINCT FROM $3
			  AND child_host_id IS NOT DISTINCT FROM $4
			  AND child_monitor_id IS NOT DISTINCT FROM $5
			  AND child_label_scope IS NOT DISTINCT FROM $6
			  AND child_label_value IS NOT DISTINCT FROM $7
		)`,
		e.ProjectID, e.ParentHostID, e.ParentMonitorID,
		e.ChildHostID, e.ChildMonitorID, e.ChildLabelScope, e.ChildLabelValue,
		excludeID,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("depsuppress: check duplicate: %w", err)
	}
	if exists {
		return fmt.Errorf("%w", ErrDuplicate)
	}
	return nil
}

// node — узел графа явных зависимостей: пара (тип, id). Label-рёбра не
// участвуют в графе — их цикличность неразрешима статически (страхует
// depth-cap на этапе применения, вне этого пакета).
type node struct {
	kind string // "host" | "monitor"
	id   int64
}

// checkCycle строит ориентированный граф узлов из существующих явных рёбер
// проекта (родитель-узел → ребёнок-узел) плюс новое ребро и проверяет, не
// достигает ли ребёнок нового ребра транзитивно родителя нового ребра
// (обычный BFS). Рёбра, где ребёнок — label-селектор, в граф не входят.
// excludeID — id редактируемого ребра при Update: его старая версия уходит
// из графа (в БД её вот-вот заменит e), иначе разворот единственного ребра
// A→B в B→A ловил бы ложный «цикл» сам с собой; 0 при Create.
func checkCycle(ctx context.Context, tx pgx.Tx, e Edge, excludeID int64) error {
	// Новое ребро — не среди явных узлов (ребёнок — label-селектор): цикл
	// среди явных узлов невозможен.
	if e.ChildHostID == nil && e.ChildMonitorID == nil {
		return nil
	}

	parent := parentNode(e)
	child := childNode(e)
	if parent == nil || child == nil {
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT parent_host_id, parent_monitor_id, child_host_id, child_monitor_id
		FROM alert_dependencies
		WHERE project_id = $1 AND id <> $2
		  AND (child_host_id IS NOT NULL OR child_monitor_id IS NOT NULL)`,
		e.ProjectID, excludeID)
	if err != nil {
		return fmt.Errorf("depsuppress: load edges for cycle check: %w", err)
	}
	defer rows.Close()

	adj := map[node][]node{}
	for rows.Next() {
		var parentHostID, parentMonitorID, childHostID, childMonitorID *int64
		if err := rows.Scan(&parentHostID, &parentMonitorID, &childHostID, &childMonitorID); err != nil {
			return fmt.Errorf("depsuppress: scan edge for cycle check: %w", err)
		}
		p := nodeFromIDs(parentHostID, parentMonitorID)
		c := nodeFromIDs(childHostID, childMonitorID)
		if p == nil || c == nil {
			continue
		}
		adj[*p] = append(adj[*p], *c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("depsuppress: load edges for cycle check: %w", err)
	}

	// Добавляем новое ребро к графу и ищем путь child -> ... -> parent (BFS).
	adj[*parent] = append(adj[*parent], *child)

	visited := map[node]bool{*child: true}
	queue := []node{*child}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == *parent {
			return fmt.Errorf("%w: edge %v -> %v closes a cycle", ErrCycle, parent, child)
		}
		for _, next := range adj[cur] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func parentNode(e Edge) *node {
	return nodeFromIDs(e.ParentHostID, e.ParentMonitorID)
}

func childNode(e Edge) *node {
	return nodeFromIDs(e.ChildHostID, e.ChildMonitorID)
}

func nodeFromIDs(hostID, monitorID *int64) *node {
	if hostID != nil {
		return &node{kind: "host", id: *hostID}
	}
	if monitorID != nil {
		return &node{kind: "monitor", id: *monitorID}
	}
	return nil
}
