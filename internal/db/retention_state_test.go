package db_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRecordRetentionReportsDisagreement: расхождение сроков хранения между
// репликами обязано быть видимым. TTL — свойство инсталляции, а задаётся
// окружением каждой реплики: две реплики с разными значениями перекидывают TTL
// туда-обратно, и каждый переброс переписывает все куски таблицы.
func TestRecordRetentionReportsDisagreement(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := db.RecordRetention(ctx, pool, map[string]int{"events": 90})
	if err != nil {
		t.Fatalf("RecordRetention: %v", err)
	}
	if len(first) != 1 || first[0].Changed() {
		t.Fatalf("первая запись = %+v, want без изменения", first)
	}

	same, err := db.RecordRetention(ctx, pool, map[string]int{"events": 90})
	if err != nil {
		t.Fatalf("RecordRetention: %v", err)
	}
	if same[0].Changed() {
		t.Errorf("совпадающее значение отмечено как изменение — предупреждение шумело бы на каждом старте")
	}

	other, err := db.RecordRetention(ctx, pool, map[string]int{"events": 14})
	if err != nil {
		t.Fatalf("RecordRetention: %v", err)
	}
	if !other[0].Changed() {
		t.Errorf("смена срока с 90 на 14 не отмечена — переброс TTL остался невидимым")
	}
	if other[0].Previous != 90 || other[0].Current != 14 {
		t.Errorf("изменение = %+v, want previous=90 current=14", other[0])
	}
}
