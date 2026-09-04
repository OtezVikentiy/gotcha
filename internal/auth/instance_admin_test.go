package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestTransferInstanceAdmin: находка K7-1 — единственный админ инстанса не
// должен остаться без пути передачи роли. A регистрируется первым (bootstrap
// делает его админом), B — обычный пользователь; TransferInstanceAdmin
// переносит флаг на B.
func TestTransferInstanceAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uidA, err := svc.Register(ctx, "transfer-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	uidB, err := svc.Register(ctx, "transfer-b@example.com", "password12")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	toUID, err := svc.TransferInstanceAdmin(ctx, uidA, "transfer-b@example.com")
	if err != nil {
		t.Fatalf("TransferInstanceAdmin: %v", err)
	}
	if toUID != uidB {
		t.Fatalf("TransferInstanceAdmin toUID = %d, want %d", toUID, uidB)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, uidA); err != nil || admin {
		t.Fatalf("A IsInstanceAdmin after transfer = (%v,%v), want (false,nil)", admin, err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, uidB); err != nil || !admin {
		t.Fatalf("B IsInstanceAdmin after transfer = (%v,%v), want (true,nil)", admin, err)
	}
}

// TestTransferInstanceAdminCaseInsensitiveEmail: email — citext, передача по
// email в другом регистре обязана резолвиться на тот же аккаунт.
func TestTransferInstanceAdminCaseInsensitiveEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uidA, err := svc.Register(ctx, "transfer-case-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	uidB, err := svc.Register(ctx, "Transfer-Case-B@Example.com", "password12")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	toUID, err := svc.TransferInstanceAdmin(ctx, uidA, "Transfer-Case-B@Example.com")
	if err != nil {
		t.Fatalf("TransferInstanceAdmin: %v", err)
	}
	if toUID != uidB {
		t.Fatalf("TransferInstanceAdmin toUID = %d, want %d", toUID, uidB)
	}
}

// TestTransferInstanceAdminRejectsUnknownEmail: неизвестный email — ErrUserNotFound,
// флаг текущего админа не трогается.
func TestTransferInstanceAdminRejectsUnknownEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uidA, err := svc.Register(ctx, "transfer-unknown-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	if _, err := svc.TransferInstanceAdmin(ctx, uidA, "nobody@example.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("TransferInstanceAdmin unknown email = %v, want ErrUserNotFound", err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after failed transfer = (%v,%v), want (true,nil)", admin, err)
	}
}

// TestTransferInstanceAdminRejectsSelf: передача самому себе бессмысленна и
// отклоняется отдельной sentinel-ошибкой, не молчаливым no-op.
func TestTransferInstanceAdminRejectsSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uidA, err := svc.Register(ctx, "transfer-self@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	if _, err := svc.TransferInstanceAdmin(ctx, uidA, "transfer-self@example.com"); !errors.Is(err, auth.ErrSelfTransfer) {
		t.Fatalf("TransferInstanceAdmin self = %v, want ErrSelfTransfer", err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after self-transfer attempt = (%v,%v), want (true,nil)", admin, err)
	}
}

// TestTransferInstanceAdminRejectsNonAdmin: B (не админ) пытается передать
// роль C — ErrNotInstanceAdmin, действующий админ A не меняется.
func TestTransferInstanceAdminRejectsNonAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uidA, err := svc.Register(ctx, "transfer-nonadmin-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	uidB, err := svc.Register(ctx, "transfer-nonadmin-b@example.com", "password12")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	if _, err := svc.Register(ctx, "transfer-nonadmin-c@example.com", "password12"); err != nil {
		t.Fatalf("Register C: %v", err)
	}

	if _, err := svc.TransferInstanceAdmin(ctx, uidB, "transfer-nonadmin-c@example.com"); !errors.Is(err, auth.ErrNotInstanceAdmin) {
		t.Fatalf("TransferInstanceAdmin from non-admin = %v, want ErrNotInstanceAdmin", err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after rejected transfer = (%v,%v), want (true,nil)", admin, err)
	}
}

// TestTransferInstanceAdminRollsBackWhenGrantFails: если вторая часть
// передачи (grant получателю) не может выполниться, вся транзакция обязана
// откатиться — A должен остаться админом, а не потерять флаг без передачи
// кому-либо. Блокируем строку B внешней транзакцией с FOR UPDATE, чтобы
// UPDATE ... WHERE id = B внутри TransferInstanceAdmin завис и упал по ctx
// timeout, и проверяем состояние после Rollback лока.
func TestTransferInstanceAdminRollsBackWhenGrantFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	bg := context.Background()

	uidA, err := svc.Register(bg, "transfer-rollback-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	uidB, err := svc.Register(bg, "transfer-rollback-b@example.com", "password12")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM users WHERE id = $1 FOR UPDATE", uidB); err != nil {
		t.Fatalf("lock B: %v", err)
	}

	transferCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if _, err := svc.TransferInstanceAdmin(transferCtx, uidA, "transfer-rollback-b@example.com"); err == nil {
		t.Fatalf("TransferInstanceAdmin with locked recipient = nil error, want timeout")
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	if admin, err := svc.UserIsInstanceAdmin(bg, uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after failed grant = (%v,%v), want (true,nil)", admin, err)
	}
	if admin, err := svc.UserIsInstanceAdmin(bg, uidB); err != nil || admin {
		t.Fatalf("B IsInstanceAdmin after failed grant = (%v,%v), want (false,nil)", admin, err)
	}
}

// TestDeleteSelfAccountLocksInstanceAdminFlag (F3, раунд правок по ревью
// финревью волны 1 аудита перед 1.0): FOR UPDATE в SELECT is_instance_admin
// сериализует DeleteSelfAccount с конкурентной grant-половиной
// TransferInstanceAdmin. Наивный тест «блокируется ли вызов вообще» мутанта
// (снятие FOR UPDATE) не отличит: без FOR UPDATE SELECT проходит РАНЬШЕ
// коммита конкурентной транзакции (читает старое значение), но последующий
// DELETE FROM users всё равно упирается в тот же лок строки — вызов виснет
// в обоих случаях. Различие — в ИТОГЕ после коммита: с FOR UPDATE SELECT
// дожидается коммита, видит is_instance_admin=true и возвращает
// ErrInstanceAdminBlocked, A не удаляется; без FOR UPDATE проверка уже
// пройдена по старому (false) значению, и DELETE, дождавшись лока, просто
// удаляет A.
//
// Сценарий: B — единственный админ инстанса (первый зарегистрированный), A —
// обычный пользователь. Внешняя транзакция делает НЕЗАКОММИЧЕННЫЙ
// UPDATE users SET is_instance_admin = true WHERE id = A (ровно grant-
// половина TransferInstanceAdmin, держит эксклюзивный лок строки A);
// конкурентно вызывается DeleteSelfAccount(A); внешняя транзакция
// коммитится.
func TestDeleteSelfAccountLocksInstanceAdminFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	bg := context.Background()

	uidB, err := svc.Register(bg, "delete-lock-b@example.com", "password12")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	uidA, err := svc.Register(bg, "delete-lock-a@example.com", "password12")
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// one_instance_admin (0017) — частичный UNIQUE на is_instance_admin: как
	// и настоящий TransferInstanceAdmin, сперва снимаем флаг у B, потом
	// ставим A — иначе конфликт индекса ещё до коммита.
	grantTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin grant tx: %v", err)
	}
	defer grantTx.Rollback(bg)
	if _, err := grantTx.Exec(bg, "UPDATE users SET is_instance_admin = false WHERE id = $1", uidB); err != nil {
		t.Fatalf("release half: %v", err)
	}
	if _, err := grantTx.Exec(bg, "UPDATE users SET is_instance_admin = true WHERE id = $1", uidA); err != nil {
		t.Fatalf("grant half: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.DeleteSelfAccount(bg, uidA)
	}()

	// DeleteSelfAccount обязана застрять на локе строки A (на SELECT ... FOR
	// UPDATE — с фиксом; на самом DELETE — без него), пока grantTx не
	// закоммичена. Короткая пауза исключает случайное прохождение раньше.
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteSelfAccount вернулась до коммита grant-транзакции (err=%v) — вызов не заблокирован на строке A", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := grantTx.Commit(bg); err != nil {
		t.Fatalf("commit grant tx: %v", err)
	}

	var deleteErr error
	select {
	case deleteErr = <-deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteSelfAccount не вернулась после коммита grant-транзакции")
	}

	if !errors.Is(deleteErr, auth.ErrInstanceAdminBlocked) {
		t.Fatalf("DeleteSelfAccount после конкурентного grant = %v, want ErrInstanceAdminBlocked", deleteErr)
	}
	var stillExists bool
	if err := pool.QueryRow(bg, "SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)", uidA).Scan(&stillExists); err != nil {
		t.Fatalf("check A exists: %v", err)
	}
	if !stillExists {
		t.Fatal("A удалён, хотя DeleteSelfAccount вернула ErrInstanceAdminBlocked")
	}
}
