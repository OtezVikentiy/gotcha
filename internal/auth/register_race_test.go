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
			<-start // отпускаем все горутины разом, чтобы усилить гонку
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
