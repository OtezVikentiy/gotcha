package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRegisterVsDeleteSelfAccountRace(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	bg := context.Background()

	const email = "race-del-reg@example.com"
	uid, err := svc.Register(bg, email, "password12")
	if err != nil {
		t.Fatalf("Register (единственный пользователь): %v", err)
	}
	if admin, err := svc.UserIsInstanceAdmin(bg, uid); err != nil || !admin {
		t.Fatalf("единственный пользователь не админ = (%v,%v), want (true,nil)", admin, err)
	}

	delTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin delTx: %v", err)
	}
	defer delTx.Rollback(bg)
	// Дублируем неэкспортированный instanceAdminBootstrapLockClass (identity.go) прямым числом.
	const instanceAdminBootstrapLockClass = 3
	if _, err := delTx.Exec(bg, "SELECT pg_advisory_xact_lock($1, 0)", instanceAdminBootstrapLockClass); err != nil {
		t.Fatalf("delTx: bootstrap lock: %v", err)
	}
	var admin bool
	if err := delTx.QueryRow(bg,
		"SELECT is_instance_admin FROM users WHERE id = $1 FOR UPDATE", uid).Scan(&admin); err != nil {
		t.Fatalf("delTx: read admin flag: %v", err)
	}
	if !admin {
		t.Fatalf("delTx: пользователь %d не админ перед удалением", uid)
	}
	var othersExist bool
	if err := delTx.QueryRow(bg,
		"SELECT EXISTS (SELECT 1 FROM users WHERE id <> $1)", uid).Scan(&othersExist); err != nil {
		t.Fatalf("delTx: othersExist: %v", err)
	}
	if othersExist {
		t.Fatalf("delTx: othersExist = true, ожидали единственного пользователя")
	}
	if _, err := delTx.Exec(bg, "DELETE FROM users WHERE id = $1", uid); err != nil {
		t.Fatalf("delTx: delete: %v", err)
	}

	registerDone := make(chan struct {
		id  int64
		err error
	}, 1)
	go func() {
		id, err := svc.Register(bg, email, "password34")
		registerDone <- struct {
			id  int64
			err error
		}{id, err}
	}()

	// До коммита delTx вызов Register возвращаться не должен — иначе блокировка не работает.
	select {
	case res := <-registerDone:
		t.Fatalf("Register вернулся до коммита delTx (id=%d, err=%v) — не заблокирован на конкурентном удалении", res.id, res.err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := delTx.Commit(bg); err != nil {
		t.Fatalf("commit delTx: %v", err)
	}

	var res struct {
		id  int64
		err error
	}
	select {
	case res = <-registerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Register не вернулся после коммита delTx")
	}
	if res.err != nil {
		t.Fatalf("Register после коммита конкурентного удаления: %v", res.err)
	}

	count, err := svc.UserCount(bg)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("UserCount после гонки = %d, want 1 (старый удалён, новый зарегистрирован)", count)
	}
	if newAdmin, err := svc.UserIsInstanceAdmin(bg, res.id); err != nil || !newAdmin {
		t.Fatalf("новый пользователь IsInstanceAdmin = (%v,%v), want (true,nil): "+
			"инстанс остался без администратора — гонка Register/DeleteSelfAccount не закрыта", newAdmin, err)
	}
}

func TestDeleteSelfAccountRespectsBootstrapLock(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "bootstrap-lock-delete@example.com", "password12")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lockTx: %v", err)
	}
	defer lockTx.Rollback(bg)
	const instanceAdminBootstrapLockClass = 3
	if _, err := lockTx.Exec(bg, "SELECT pg_advisory_xact_lock($1, 0)", instanceAdminBootstrapLockClass); err != nil {
		t.Fatalf("lockTx: bootstrap lock: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.DeleteSelfAccount(bg, uid)
	}()

	// Ждём ту же блокировку, что держит lockTx — иначе лок не сериализовал бы конкурентные вызовы.
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteSelfAccount вернулась до отпускания lockTx (err=%v) — не сериализована общим локом", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := lockTx.Commit(bg); err != nil {
		t.Fatalf("commit lockTx: %v", err)
	}

	var delErr error
	select {
	case delErr = <-deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteSelfAccount не вернулась после отпускания lockTx")
	}
	if delErr != nil {
		t.Fatalf("DeleteSelfAccount после отпускания lockTx: %v", delErr)
	}

	count, err := svc.UserCount(bg)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("UserCount после удаления = %d, want 0", count)
	}
}
