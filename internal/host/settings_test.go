package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// setupSettingsProject — своя заготовка (не переиспользует setupProject из
// host_test.go): slug должен отличаться, иначе UNIQUE(slug) организаций
// столкнёт два теста, случайно запущенных в одном пакете.
func setupSettingsProject(t *testing.T) (*host.SettingsService, int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('host-settings', 'Host Settings', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-settings', 'Host Settings') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return host.NewSettingsService(pool), projectID
}

// TestSettingsServiceGetWithoutRowReturnsDefaults — строки для проекта ещё
// нет (ленивое создание при первом Save) → Get отдаёт DefaultSettings(), а
// не ошибку.
func TestSettingsServiceGetWithoutRowReturnsDefaults(t *testing.T) {
	svc, projectID := setupSettingsProject(t)
	ctx := context.Background()

	got, err := svc.Get(ctx, projectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != host.DefaultSettings() {
		t.Fatalf("Get без строки = %+v, want DefaultSettings() = %+v", got, host.DefaultSettings())
	}
}

// TestSettingsServiceSaveThenGetRoundTrips — Save нестандартных значений →
// Get возвращает ровно их же (не дефолты).
func TestSettingsServiceSaveThenGetRoundTrips(t *testing.T) {
	svc, projectID := setupSettingsProject(t)
	ctx := context.Background()

	want := host.Settings{
		DiskEnabled:     true,
		DiskThreshold:   0.5,
		MemoryEnabled:   false,
		MemoryThreshold: 0.75,
		LoadEnabled:     true,
		LoadThreshold:   3.5,
		SilentEnabled:   true,
		SilentAfter:     240 * time.Second,
	}
	if err := svc.Save(ctx, projectID, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := svc.Get(ctx, projectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Fatalf("Get после Save = %+v, want %+v", got, want)
	}
}

// TestSettingsServiceGetWithExists — признак наличия строки (M2): без Save
// exists=false и DefaultSettings(), после Save — exists=true и сохранённые
// значения (даже если они совпадают с дефолтом).
func TestSettingsServiceGetWithExists(t *testing.T) {
	svc, projectID := setupSettingsProject(t)
	ctx := context.Background()

	got, exists, err := svc.GetWithExists(ctx, projectID)
	if err != nil {
		t.Fatalf("GetWithExists без строки: %v", err)
	}
	if exists {
		t.Fatalf("GetWithExists без строки: exists = true, want false")
	}
	if got != host.DefaultSettings() {
		t.Fatalf("GetWithExists без строки = %+v, want DefaultSettings()", got)
	}

	if err := svc.Save(ctx, projectID, host.DefaultSettings()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, exists, err = svc.GetWithExists(ctx, projectID)
	if err != nil {
		t.Fatalf("GetWithExists после Save: %v", err)
	}
	if !exists {
		t.Fatalf("GetWithExists после Save: exists = false, want true")
	}
	if got != host.DefaultSettings() {
		t.Fatalf("GetWithExists после Save = %+v, want DefaultSettings()", got)
	}
}

// TestSettingsServiceSaveUpsertsOnSecondCall — повторный Save того же
// проекта обновляет строку (ON CONFLICT DO UPDATE), а не плодит вторую и не
// падает на PK-конфликте.
func TestSettingsServiceSaveUpsertsOnSecondCall(t *testing.T) {
	svc, projectID := setupSettingsProject(t)
	ctx := context.Background()

	first := host.DefaultSettings()
	first.DiskThreshold = 0.6
	if err := svc.Save(ctx, projectID, first); err != nil {
		t.Fatalf("Save #1: %v", err)
	}

	second := first
	second.DiskThreshold = 0.8
	second.SilentAfter = 600 * time.Second
	if err := svc.Save(ctx, projectID, second); err != nil {
		t.Fatalf("Save #2: %v", err)
	}

	got, err := svc.Get(ctx, projectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != second {
		t.Fatalf("Get после Save #2 = %+v, want %+v", got, second)
	}
}

// TestSettingsServiceSaveRejectsInvalid — Save с невалидными значениями не
// должен долетать до БД: Validate отсекает их раньше и с различимой
// ошибкой (нужно для FormState — какое конкретно поле подсветить).
func TestSettingsServiceSaveRejectsInvalid(t *testing.T) {
	svc, projectID := setupSettingsProject(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(s *host.Settings)
		wantErr error
	}{
		{
			name:    "silent ниже минимума 180с",
			mutate:  func(s *host.Settings) { s.SilentAfter = 120 * time.Second },
			wantErr: host.ErrInvalidSilentAfter,
		},
		{
			name:    "silent ровно на границе минус миллисекунда — тоже ошибка",
			mutate:  func(s *host.Settings) { s.SilentAfter = 179 * time.Second },
			wantErr: host.ErrInvalidSilentAfter,
		},
		{
			// Верхняя граница: порог, сравнимый с окном активных хостов
			// (сутки), не сработал бы никогда, а введённое в форму «много»
			// переполняло бы колонку int4 пятисоткой вместо 422.
			name:    "silent выше максимума 12ч",
			mutate:  func(s *host.Settings) { s.SilentAfter = 13 * time.Hour },
			wantErr: host.ErrInvalidSilentAfter,
		},
		{
			name:    "диск выше 1.0",
			mutate:  func(s *host.Settings) { s.DiskThreshold = 1.5 },
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			name:    "диск = 0",
			mutate:  func(s *host.Settings) { s.DiskThreshold = 0 },
			wantErr: host.ErrInvalidDiskThreshold,
		},
		{
			name:    "память выше 1.0",
			mutate:  func(s *host.Settings) { s.MemoryThreshold = 1.01 },
			wantErr: host.ErrInvalidMemoryThreshold,
		},
		{
			name:    "load = 0",
			mutate:  func(s *host.Settings) { s.LoadThreshold = 0 },
			wantErr: host.ErrInvalidLoadThreshold,
		},
		{
			name:    "load отрицательный",
			mutate:  func(s *host.Settings) { s.LoadThreshold = -1 },
			wantErr: host.ErrInvalidLoadThreshold,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := host.DefaultSettings()
			tc.mutate(&s)

			err := svc.Save(ctx, projectID, s)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Save(%+v) err = %v, want errors.Is(_, %v)", s, err, tc.wantErr)
			}

			// Отвергнутый Save не должен создать строку — следующий Get всё
			// ещё обязан отдавать дефолты.
			got, getErr := svc.Get(ctx, projectID)
			if getErr != nil {
				t.Fatalf("Get после отвергнутого Save: %v", getErr)
			}
			if got != host.DefaultSettings() {
				t.Fatalf("Get после отвергнутого Save = %+v, want DefaultSettings()", got)
			}
		})
	}
}

// TestSettingsServiceValidateAcceptsBoundaries — граничные значения (ровно
// на CHECK'ах) обязаны проходить: 180с силент, диск/память=1.0, минимальный
// положительный load.
func TestSettingsServiceValidateAcceptsBoundaries(t *testing.T) {
	s := host.DefaultSettings()
	s.SilentAfter = host.MinSilentAfter
	s.DiskThreshold = 1.0
	s.MemoryThreshold = 1.0
	s.LoadThreshold = 0.01

	if err := host.Validate(s); err != nil {
		t.Fatalf("Validate(граничные значения) = %v, want nil", err)
	}
}

// TestMinSilentAfterValue — константа зафиксирована в брифе как 180с
// (инвариант ≥3× троттлинга Toucher 60с); тест ловит случайную правку числа.
func TestMinSilentAfterValue(t *testing.T) {
	if host.MinSilentAfter != 180*time.Second {
		t.Fatalf("MinSilentAfter = %v, want 180s", host.MinSilentAfter)
	}
}

// TestKindEnabledKnowsEveryKind — сторож ревью I2: KindEnabled обязан знать
// КАЖДЫЙ вид из Kinds. Незнакомый вид отдаёт ok=false, и вызывающий его
// пропускает — то есть новый вид, добавленный в Kinds без строки в switch,
// молча выпал бы из «закрыть инциденты выключенного порога», а не сломался
// заметно.
func TestKindEnabledKnowsEveryKind(t *testing.T) {
	s := host.DefaultSettings()
	for _, kind := range host.Kinds {
		enabled, ok := s.KindEnabled(kind)
		if !ok {
			t.Errorf("KindEnabled(%q): ok = false — вид есть в Kinds, но не разобран", kind)
			continue
		}
		if !enabled {
			t.Errorf("KindEnabled(%q) = false при DefaultSettings (все пороги включены по умолчанию)", kind)
		}
	}
	if _, ok := s.KindEnabled("swap"); ok {
		t.Error(`KindEnabled("swap"): ok = true для вида, которого нет в Kinds`)
	}
}

// TestKindEnabledFollowsFlags — KindEnabled читает именно свой флаг вида, а не
// соседний (перепутанный case в switch иначе прошёл бы незамеченным).
func TestKindEnabledFollowsFlags(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		disable func(*host.Settings)
	}{
		{"disk", func(s *host.Settings) { s.DiskEnabled = false }},
		{"memory", func(s *host.Settings) { s.MemoryEnabled = false }},
		{"load", func(s *host.Settings) { s.LoadEnabled = false }},
		{"silent", func(s *host.Settings) { s.SilentEnabled = false }},
	} {
		s := host.DefaultSettings()
		tc.disable(&s)
		for _, kind := range host.Kinds {
			enabled, ok := s.KindEnabled(kind)
			if !ok {
				t.Fatalf("KindEnabled(%q): ok = false", kind)
			}
			want := kind != tc.kind
			if enabled != want {
				t.Errorf("выключен только %q: KindEnabled(%q) = %v, want %v", tc.kind, kind, enabled, want)
			}
		}
	}
}
