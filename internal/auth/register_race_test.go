package auth_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegister_ConcurrentFirstAdminRace (audit H8) — the first-user-is-admin
// bootstrap (PROD-B1) must grant instance-admin to AT MOST ONE user even when
// many registrations race on an empty instance. Each Register computes the
// admin flag as `NOT EXISTS (SELECT 1 FROM users)`.
//
// T8 фикс-раунд 1: до instanceAdminBootstrapLockClass (identity.go) несколько
// горутин могли увидеть пустую таблицу РАЗОМ, и только один true-insert
// проходил партиальный уникальный индекс one_instance_admin — проигравшие
// ловили 23505 и ретраили как не-админы (RA-L6). Этот тест раньше проверял
// именно этот ретрай (которого TestRegister_FirstUserIsInstanceAdmin,
// последовательный, не задевал). Сейчас лок сериализует все n регистраций
// между собой: каждая ждёт своей очереди и видит уже закоммиченный результат
// предыдущей, поэтому 23505 по one_instance_admin здесь больше не возникает
// (ретрай-ветка в Register убрана как мёртвый код — 0 попаданий на этом
// тесте после лока). Что тест по-прежнему проверяет — сам ИНВАРИАНТ, ради
// которого раньше существовал ретрай: ровно один инстанс-админ переживает
// конкурентную первую регистрацию, ни одна регистрация не теряется.
//
// A regression that dropped the bootstrap lock would surface here as either
// two instance admins (privilege escalation, if the old race reopens) or as
// a hard "unexpected one_instance_admin conflict" error from Register (see
// its docblock) instead of a clean result.
func TestRegister_ConcurrentFirstAdminRace(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const n = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		ids   []int64
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			email := fmt.Sprintf("race-%d@example.com", i)
			<-start // release all goroutines at once to maximize the race
			id, err := svc.Register(ctx, email, "password12")
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				ids = append(ids, id)
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("concurrent Register errors = %v, want none (all %d must persist)", errs, n)
	}
	if len(ids) != n {
		t.Fatalf("persisted users = %d, want %d", len(ids), n)
	}
	if count, err := svc.UserCount(ctx); err != nil || count != n {
		t.Fatalf("UserCount = (%d,%v), want (%d,nil)", count, err, n)
	}

	// Exactly one instance admin across all registered users.
	admins := 0
	for _, id := range ids {
		isAdmin, err := svc.UserIsInstanceAdmin(ctx, id)
		if err != nil {
			t.Fatalf("UserIsInstanceAdmin(%d): %v", id, err)
		}
		if isAdmin {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("instance admins after concurrent first registration = %d, want exactly 1", admins)
	}
}
