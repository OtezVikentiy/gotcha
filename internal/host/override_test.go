package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// insertProjectHT — org+project для тестов override/group-thresholds; свой
// slug (параметр), чтобы не столкнуться с setupProject/setupSettingsProject
// других файлов пакета по UNIQUE(slug) при параллельном запуске.
func insertProjectHT(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 0) RETURNING id", slug).
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id", orgID, slug).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

// TestHostOverrideGetSave — пустой override для хоста без строки (всё nil),
// частичный override после Save (некоторые поля пришпилены, остальные
// по-прежнему nil = наследовать), и батч-выборка GetForHosts.
func TestHostOverrideGetSave(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	proj := insertProjectHT(t, pool, "ov")
	var hostID int64
	if err := pool.QueryRow(ctx, "INSERT INTO hosts (project_id,name) VALUES ($1,'h1') RETURNING id", proj).Scan(&hostID); err != nil {
		t.Fatalf("host: %v", err)
	}

	svc := host.NewHostOverrideService(pool)

	// Пусто → все nil.
	ov, err := svc.Get(ctx, hostID)
	if err != nil || ov.DiskEnabled != nil {
		t.Fatalf("empty override: %v %+v", err, ov)
	}

	// Частичный: disk on 0.8, silent off (без значения — разрешено, см.
	// ValidateOverride: enabled=false не требует value).
	on := true
	off := false
	dv := 0.80
	if err := svc.Save(ctx, hostID, host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &dv, SilentEnabled: &off}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := svc.Get(ctx, hostID)
	if err != nil {
		t.Fatalf("get after save: %v", err)
	}
	if got.DiskEnabled == nil || *got.DiskEnabled != true || got.DiskThreshold == nil || *got.DiskThreshold != 0.80 ||
		got.SilentEnabled == nil || *got.SilentEnabled != false || got.MemoryEnabled != nil {
		t.Fatalf("override roundtrip: %+v", got)
	}

	// Батч.
	m, err := svc.GetForHosts(ctx, []int64{hostID})
	if err != nil || m[hostID].DiskThreshold == nil {
		t.Fatalf("batch: %v %+v", err, m)
	}
}

// TestHostOverrideSaveRejectsInvalid — Save проверяет override через
// ValidateOverride до записи: несогласованный или out-of-bounds override не
// должен долетать до БД (и, соответственно, не должен создавать строку).
func TestHostOverrideSaveRejectsInvalid(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	proj := insertProjectHT(t, pool, "ov-bad")
	var hostID int64
	if err := pool.QueryRow(ctx, "INSERT INTO hosts (project_id,name) VALUES ($1,'h1') RETURNING id", proj).Scan(&hostID); err != nil {
		t.Fatalf("host: %v", err)
	}
	svc := host.NewHostOverrideService(pool)

	on := true
	bad := 2.0 // диск вне (0,1]
	err := svc.Save(ctx, hostID, host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &bad})
	if !errors.Is(err, host.ErrInvalidDiskThreshold) {
		t.Fatalf("Save(вне границ) err = %v, want errors.Is(_, ErrInvalidDiskThreshold)", err)
	}

	got, getErr := svc.Get(ctx, hostID)
	if getErr != nil {
		t.Fatalf("get after rejected save: %v", getErr)
	}
	if got.DiskEnabled != nil {
		t.Fatalf("отвергнутый Save создал строку: %+v", got)
	}
}

// TestGroupThresholdListUpsertDelete — List пуст без строк, Upsert создаёт
// строку и обновляет её при повторном вызове (тот же ключ project/scope/
// label), Delete удаляет.
func TestGroupThresholdListUpsertDelete(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	proj := insertProjectHT(t, pool, "grp")
	svc := host.NewGroupThresholdService(pool)

	got, err := svc.List(ctx, proj)
	if err != nil || len(got) != 0 {
		t.Fatalf("List пустой проект: %v %+v", err, got)
	}

	on := true
	load := 1.5
	if err := svc.Upsert(ctx, proj, "env", "prod", host.ThresholdOverride{LoadEnabled: &on, LoadThreshold: &load}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err = svc.List(ctx, proj)
	if err != nil || len(got) != 1 {
		t.Fatalf("List после upsert: %v %+v", err, got)
	}
	if got[0].Scope != "env" || got[0].Label != "prod" || got[0].LoadThreshold == nil || *got[0].LoadThreshold != 1.5 {
		t.Fatalf("group threshold roundtrip: %+v", got[0])
	}

	// Повторный Upsert того же ключа — обновляет, не плодит вторую строку.
	load2 := 2.5
	if err := svc.Upsert(ctx, proj, "env", "prod", host.ThresholdOverride{LoadEnabled: &on, LoadThreshold: &load2}); err != nil {
		t.Fatalf("upsert #2: %v", err)
	}
	got, err = svc.List(ctx, proj)
	if err != nil || len(got) != 1 || got[0].LoadThreshold == nil || *got[0].LoadThreshold != 2.5 {
		t.Fatalf("List после upsert #2: %v %+v", err, got)
	}

	if err := svc.Delete(ctx, proj, "env", "prod"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = svc.List(ctx, proj)
	if err != nil || len(got) != 0 {
		t.Fatalf("List после delete: %v %+v", err, got)
	}

	// Delete отсутствующей строки — не ошибка (идемпотентен).
	if err := svc.Delete(ctx, proj, "env", "prod"); err != nil {
		t.Fatalf("delete повторно: %v", err)
	}
}

// TestValidateOverride — таблица сочетаний enabled/value для одного вида
// (disk) и границы silent; остальные виды (memory/load) проверены через тот
// же validateKindOverride и не дублируются построчно.
func TestValidateOverride(t *testing.T) {
	on := true
	off := false
	valid := 0.5
	invalid := 1.5
	dead := 1.0

	cases := []struct {
		name    string
		ov      host.ThresholdOverride
		wantErr error
	}{
		{
			name: "всё nil — валидно (полностью наследуется)",
			ov:   host.ThresholdOverride{},
		},
		{
			name: "enabled=true + value валиден — ок",
			ov:   host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &valid},
		},
		{
			name: "enabled=false без value — ок (выключить без пришпиленного значения)",
			ov:   host.ThresholdOverride{DiskEnabled: &off},
		},
		{
			name:    "M-3: value без enabled — ошибка",
			ov:      host.ThresholdOverride{DiskThreshold: &valid},
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			name:    "enabled=true без value — ошибка",
			ov:      host.ThresholdOverride{DiskEnabled: &on},
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			name:    "value вне границ, даже при enabled=false — ошибка",
			ov:      host.ThresholdOverride{DiskEnabled: &off, DiskThreshold: &invalid},
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			// Как в Validate: 1.0 (100%) со строгим «>» оценщика не сработал бы
			// никогда — переопределение с таким значением отвергается (K3-2).
			name:    "disk: ровно 1.0 — мёртвый порог, ошибка",
			ov:      host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &dead},
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			name:    "memory: ровно 1.0 — мёртвый порог, ошибка",
			ov:      host.ThresholdOverride{MemoryEnabled: &on, MemoryThreshold: &dead},
			wantErr: host.ErrInvalidMemoryThreshold,
		},
		{
			name: "silent: enabled=false без value — ок",
			ov:   host.ThresholdOverride{SilentEnabled: &off},
		},
		{
			name:    "silent: value без enabled — ошибка",
			ov:      host.ThresholdOverride{SilentAfter: durPtr(200 * time.Second)},
			wantErr: host.ErrInvalidSilentAfter,
		},
		{
			name:    "silent: value ниже минимума — ошибка",
			ov:      host.ThresholdOverride{SilentEnabled: &on, SilentAfter: durPtr(60 * time.Second)},
			wantErr: host.ErrInvalidSilentAfter,
		},
		{
			name: "silent: enabled=true + валидное value — ок",
			ov:   host.ThresholdOverride{SilentEnabled: &on, SilentAfter: durPtr(200 * time.Second)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := host.ValidateOverride(tc.ov)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateOverride(%+v) = %v, want nil", tc.ov, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateOverride(%+v) = %v, want errors.Is(_, %v)", tc.ov, err, tc.wantErr)
			}
		})
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }
