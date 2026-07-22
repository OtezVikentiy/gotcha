package org

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

var (
	ErrNotMember = errors.New("org: user is not a member")
	ErrLastOwner = errors.New("org: cannot demote or remove the last owner")
	// ErrOwnerOnly — только owner может выдать роль owner, изменить роль
	// существующего owner'а или удалить owner'а (см. checkOwnerLevelGuard).
	ErrOwnerOnly = errors.New("org: only an owner can manage owner-level access")
)

// ErrInvalidRole (см. invite.go) переиспользуется здесь: роль вне
// owner/admin/member для AddMember/SetRole — тот же класс ошибки.

// validRole проверяет роль до похода в БД: owner/admin/member — единственные
// допустимые значения (CHECK-ограничение в БД дублирует это на всякий случай).
func validRole(r Role) bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember:
		return true
	default:
		return false
	}
}

// Role возвращает роль пользователя в организации.
func (s *Service) Role(ctx context.Context, orgID, userID int64) (Role, error) {
	var r Role
	err := s.pool.QueryRow(ctx,
		"SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2",
		orgID, userID).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", fmt.Errorf("org: role: %w", err)
	}
	return r, nil
}

// AddMember добавляет пользователя в организацию с ролью.
func (s *Service) AddMember(ctx context.Context, orgID, userID int64, role Role) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)",
		orgID, userID, role); err != nil {
		return fmt.Errorf("org: add member: %w", err)
	}
	return nil
}

// EnsureMember идемпотентно добавляет участника (для JIT-провижининга SSO,
// этап 10): если участник уже есть — не ошибка и роль НЕ меняется (ON CONFLICT
// DO NOTHING). Не понижаем/повышаем существующего.
func (s *Service) EnsureMember(ctx context.Context, orgID, userID int64, role Role) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT (org_id, user_id) DO NOTHING",
		orgID, userID, role); err != nil {
		return fmt.Errorf("org: ensure member: %w", err)
	}
	return nil
}

// SetRole меняет роль участника. Последнего owner понизить нельзя.
func (s *Service) SetRole(ctx context.Context, orgID, userID int64, role Role) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: set role: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if role != RoleOwner {
		if err := ensureNotLastOwner(ctx, tx, orgID, userID); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx,
		"UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2",
		orgID, userID, role)
	if err != nil {
		return fmt.Errorf("org: set role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: set role: %w", err)
	}
	return nil
}

// RemoveMember убирает участника. Последнего owner убрать нельзя.
func (s *Service) RemoveMember(ctx context.Context, orgID, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: remove member: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := ensureNotLastOwner(ctx, tx, orgID, userID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		"DELETE FROM org_members WHERE org_id = $1 AND user_id = $2", orgID, userID)
	if err != nil {
		return fmt.Errorf("org: remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: remove member: %w", err)
	}
	return nil
}

// SetRoleAs меняет роль участника targetID от имени actorID — актёрозависимый
// вариант SetRole, закрывающий TOCTOU в owner-guard'е (security fix): в
// исходной реализации веб-слой проверял роль актёра и цели отдельным
// запросом, а сама мутация выполнялась в отдельной транзакции — конкурентная
// легитимная промоция могла проскочить между проверкой и записью и позволить
// admin'у понизить свежепромоутнутого owner'а. Здесь актёр и цель читаются с
// FOR UPDATE в той же транзакции, что и last-owner проверка и сама мутация,
// поэтому конкурентные операции над одной организацией сериализуются.
func (s *Service) SetRoleAs(ctx context.Context, orgID, actorID, targetID int64, role Role) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: set role as: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkOwnerLevelGuard(ctx, tx, orgID, actorID, targetID, role); err != nil {
		return err
	}
	if role != RoleOwner {
		if err := ensureNotLastOwner(ctx, tx, orgID, targetID); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx,
		"UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2",
		orgID, targetID, role)
	if err != nil {
		return fmt.Errorf("org: set role as: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: set role as: %w", err)
	}
	return nil
}

// RemoveMemberAs убирает участника targetID от имени actorID — актёрозависимый
// вариант RemoveMember, тот же TOCTOU-фикс, что и у SetRoleAs (см. её
// комментарий): actorID и requestedRole (пустая строка — роль не
// запрашивается, действие не про смену роли) идут в checkOwnerLevelGuard в
// одной транзакции с last-owner проверкой и самим удалением.
func (s *Service) RemoveMemberAs(ctx context.Context, orgID, actorID, targetID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: remove member as: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkOwnerLevelGuard(ctx, tx, orgID, actorID, targetID, ""); err != nil {
		return err
	}
	if err := ensureNotLastOwner(ctx, tx, orgID, targetID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		"DELETE FROM org_members WHERE org_id = $1 AND user_id = $2", orgID, targetID)
	if err != nil {
		return fmt.Errorf("org: remove member as: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: remove member as: %w", err)
	}
	return nil
}

// checkOwnerLevelGuard читает роли actorID и targetID одним запросом с FOR
// UPDATE (внутри переданной транзакции — той же, что затем делает last-owner
// проверку и мутацию) и проверяет привилегию эскалации: актёр обязан быть
// owner или admin (иначе ErrNotMember — тот же класс ошибки, что и у
// отсутствующего участника), а если актёр не owner, то ни выдать роль owner
// (requestedRole), ни тронуть уже существующего owner'а (targetRole) нельзя —
// ErrOwnerOnly. requestedRole — пустая строка для RemoveMemberAs, где
// запрошенной роли нет.
func checkOwnerLevelGuard(ctx context.Context, tx pgx.Tx, orgID, actorID, targetID int64, requestedRole Role) error {
	rows, err := tx.Query(ctx, `
		SELECT user_id, role FROM org_members
		WHERE org_id = $1 AND user_id = ANY($2)
		ORDER BY user_id
		FOR UPDATE`, orgID, []int64{actorID, targetID})
	if err != nil {
		return fmt.Errorf("org: owner-level guard: %w", err)
	}
	defer rows.Close()

	roles := make(map[int64]Role, 2)
	for rows.Next() {
		var uid int64
		var r Role
		if err := rows.Scan(&uid, &r); err != nil {
			return fmt.Errorf("org: owner-level guard: %w", err)
		}
		roles[uid] = r
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("org: owner-level guard: %w", err)
	}

	actorRole, ok := roles[actorID]
	if !ok || (actorRole != RoleOwner && actorRole != RoleAdmin) {
		return ErrNotMember
	}
	targetRole, ok := roles[targetID]
	if !ok {
		return ErrNotMember
	}
	if actorRole != RoleOwner && (requestedRole == RoleOwner || targetRole == RoleOwner) {
		return ErrOwnerOnly
	}
	return nil
}

// Member — участник организации вместе с ролью; нужен UI для выбора
// ответственного за issue (страница issue, план 4).
type Member struct {
	UserID int64
	Email  string
	Role   Role
}

// MembersOf возвращает участников организации, отсортированных по email —
// стабильный порядок для <select> на странице issue.
func (s *Service) MembersOf(ctx context.Context, orgID int64) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, m.role
		FROM org_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1
		ORDER BY u.email`, orgID)
	if err != nil {
		return nil, fmt.Errorf("org: members of: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role); err != nil {
			return nil, fmt.Errorf("org: members of: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: members of: %w", err)
	}
	return out, nil
}

// ensureNotLastOwner: операция запрещена, если userID — единственный owner.
// Лочит owner-строки организации (FOR UPDATE), чтобы конкурентные демоции
// сериализовались и не могли оставить организацию без owner'а.
func ensureNotLastOwner(ctx context.Context, tx pgx.Tx, orgID, userID int64) error {
	var isOwner bool
	var owners int
	err := tx.QueryRow(ctx, `
		SELECT coalesce(bool_or(user_id = $2), false), count(*)
		FROM (SELECT user_id FROM org_members
		      WHERE org_id = $1 AND role = 'owner' FOR UPDATE) o`,
		orgID, userID).Scan(&isOwner, &owners)
	if err != nil {
		return fmt.Errorf("org: owner check: %w", err)
	}
	if isOwner && owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// SoleOwnedOrgNames возвращает названия организаций, где userID — ЕДИНСТВЕННЫЙ
// владелец. Удаление такого пользователя осиротило бы организацию (не осталось
// бы ни одного владельца), поэтому самоудаление аккаунта блокируется, пока
// такие есть: сначала передать владение или удалить организацию.
func (s *Service) SoleOwnedOrgNames(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.name
		FROM organizations o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $1 AND m.role = 'owner'
		WHERE (SELECT count(*) FROM org_members m2 WHERE m2.org_id = o.id AND m2.role = 'owner') = 1
		ORDER BY o.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("org: sole owned orgs: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("org: sole owned orgs: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
