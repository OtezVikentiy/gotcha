package depsuppress

// Тест кеша-на-тик живёт в package depsuppress (не depsuppress_test):
// getSnapshot и поле now — неэкспортируемые, а now вводился в дизайне
// (MINOR-8) именно ради тестируемости TTL — без доступа к нему изнутри
// пакета протухание снимка нечем проверить. Минимально-инвазивный путь:
// прямая композитная инициализация Suppressor{} с подменённым now, без
// новых экспортируемых/неэкспортируемых конструкторов.

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestSnapshotCacheTTL проверяет: (1) в пределах cacheTTL повторный
// getSnapshot переиспользует тот же снимок и не видит изменения, случившиеся
// в БД после первой загрузки; (2) после продвижения часов за cacheTTL
// снимок перезагружается и видит новые данные. Заодно ловит инверсию
// сравнения now().Sub(loadedAt) < cacheTTL — при инвертированном условии
// либо кеш никогда бы не переиспользовался (шаг 1 не прошёл бы), либо
// никогда не перезагружался (шаг 2 не прошёл бы).
func TestSnapshotCacheTTL(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID, projectID, hostID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,0) RETURNING id`,
		"ds-cache-ttl").Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (org_id,slug,name) VALUES ($1,$2,$2) RETURNING id`,
		orgID, "ds-cache-ttl").Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (project_id,name,environment,role) VALUES ($1,'h1','prod','web') RETURNING id`,
		projectID).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	clock := time.Now()
	sup := &Suppressor{
		pool:     pool,
		cacheTTL: cacheTTL,
		now:      func() time.Time { return clock },
	}

	snap1, err := sup.getSnapshot(ctx)
	if err != nil {
		t.Fatalf("getSnapshot #1: %v", err)
	}
	if snap1.downHosts[hostID] {
		t.Fatal("до открытия инцидента хост не должен быть в downHosts")
	}

	// Открываем silent-инцидент хоста — в пределах TTL кеш обязан остаться
	// прежним: второй вызов НЕ перезагружает снимок и НЕ видит новые данные.
	if _, err := pool.Exec(ctx,
		`INSERT INTO host_incidents (project_id,host_id,kind,status,started_at)
		 VALUES ($1,$2,'silent','open',now())`,
		projectID, hostID); err != nil {
		t.Fatalf("open incident: %v", err)
	}

	snap2, err := sup.getSnapshot(ctx)
	if err != nil {
		t.Fatalf("getSnapshot #2 (within TTL): %v", err)
	}
	if snap2 != snap1 {
		t.Fatal("в пределах TTL getSnapshot обязан вернуть ТОТ ЖЕ снимок (без перезагрузки)")
	}
	if snap2.downHosts[hostID] {
		t.Fatal("в пределах TTL кеш не должен видеть только что открытый инцидент")
	}

	// Продвигаем часы за TTL — теперь обязана произойти перезагрузка.
	clock = clock.Add(cacheTTL + time.Second)

	snap3, err := sup.getSnapshot(ctx)
	if err != nil {
		t.Fatalf("getSnapshot #3 (after TTL): %v", err)
	}
	if snap3 == snap1 {
		t.Fatal("после истечения TTL getSnapshot обязан перезагрузить снимок (новый указатель)")
	}
	if !snap3.downHosts[hostID] {
		t.Fatal("после перезагрузки кеш обязан увидеть открытый инцидент")
	}
}
