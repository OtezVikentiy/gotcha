package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegisterVsDeleteSelfAccountRace (хвост волны 1, T8) — единственный
// пользователь инстанса (админ) самоудаляется, а конкурентно кто-то
// регистрируется тем же email. Без instanceAdminBootstrapLockClass
// (identity.go/user.go) это оставляло инстанс вовсе без администратора:
// Register вычисляет is_instance_admin как `NOT EXISTS (SELECT 1 FROM
// users)` ОДНИМ оператором — если снапшот для этого вычисления берётся ДО
// того, как DeleteSelfAccount успевает закоммитить DELETE своей строки (но
// сама вставка блокируется на UNIQUE(email) до его коммита, потому что email
// совпадает), INSERT дожидается коммита и проходит уже по опустевшей
// таблице — но с уже вычисленным (устаревшим) is_instance_admin=false.
// Итог без лока: ровно один пользователь в базе, и ни один не админ.
//
// Тест не полагается на угадывание тайминга: «удаляющую» сторону гонки
// ведём вручную той же SQL-последовательностью, что и DeleteSelfAccount
// (identity.go), и держим её транзакцию открытой явным сигналом (канал), а
// не паузой — эквивалент шага DeleteSelfAccount «между SELECT и COMMIT» из
// её собственного докблока. «Регистрирующую» сторону — настоящий
// svc.Register. Блокировку конкурентного Register на этом шаге проверяем
// тем же приёмом, что TestDeleteSelfAccountLocksInstanceAdminFlag
// (instance_admin_test.go): select с таймаутом, а не сон вслепую.
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

	// "Удаляющая" сторона гонки — вручную, той же последовательностью
	// операторов, что DeleteSelfAccount: лок → FOR UPDATE → othersExist →
	// DELETE. Транзакция держится открытой до сигнала commitDelete —
	// ровно та точка «между SELECT и COMMIT», из-за которой существует эта
	// гонка.
	delTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin delTx: %v", err)
	}
	defer delTx.Rollback(bg)
	// classID=3 — instanceAdminBootstrapLockClass (identity.go), неэкспортирован
	// из пакета auth: дублируем числом с той же оговоркой, что и там (отдельно
	// от enqueueLockClassProject/enqueueLockClassUser в export/store.go).
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

	// Конкурентная регистрация ТЕМ ЖЕ email — реальный Register.
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

	// Register обязан застрять — либо на instanceAdminBootstrapLockClass
	// (с фиксом: делTx держит тот же лок), либо на UNIQUE(email) с
	// незакоммiченным DELETE (без фикса) — в обоих случаях раньше коммита
	// delTx он вернуться не должен.
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
