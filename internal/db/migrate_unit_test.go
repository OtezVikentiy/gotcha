package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

func TestPgx5URL(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "postgres scheme",
			dsn:  "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
			want: "pgx5://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
		},
		{
			name: "postgresql scheme",
			dsn:  "postgresql://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
			want: "pgx5://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
		},
		{
			name: "non-postgres string passed through unchanged",
			dsn:  "clickhouse://localhost:9000/gotcha",
			want: "clickhouse://localhost:9000/gotcha",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgx5URL(tc.dsn); got != tc.want {
				t.Errorf("pgx5URL(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestExplainMigrateErr(t *testing.T) {
	// nil остаётся nil.
	if err := explainMigrateErr("migrations/pg", nil); err != nil {
		t.Errorf("explainMigrateErr(nil) = %v, want nil", err)
	}

	// ErrNoChange трактуется как «нечего применять» → nil.
	if err := explainMigrateErr("migrations/pg", migrate.ErrNoChange); err != nil {
		t.Errorf("explainMigrateErr(ErrNoChange) = %v, want nil", err)
	}

	// dirty-состояние: внятный текст + сохранённая исходная ошибка.
	dirty := migrate.ErrDirty{Version: 5}
	got := explainMigrateErr("migrations/pg", dirty)
	if got == nil {
		t.Fatal("explainMigrateErr(ErrDirty) = nil, want error")
	}
	msg := got.Error()
	if !strings.Contains(strings.ToLower(msg), "dirty") {
		t.Errorf("сообщение не содержит \"dirty\": %q", msg)
	}
	if !strings.Contains(msg, "5") {
		t.Errorf("сообщение не содержит номер версии 5: %q", msg)
	}
	// %w сохраняет исходную ErrDirty — errors.As должен её достать.
	var derr migrate.ErrDirty
	if !errors.As(got, &derr) {
		t.Fatal("errors.As не нашёл ErrDirty в обёртке")
	}
	if derr.Version != 5 {
		t.Errorf("ErrDirty.Version = %d, want 5", derr.Version)
	}

	// Произвольная ошибка тоже оборачивается и сохраняется через %w.
	sentinel := errors.New("boom")
	wrapped := explainMigrateErr("migrations/ch", sentinel)
	if wrapped == nil {
		t.Fatal("explainMigrateErr(sentinel) = nil, want error")
	}
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is не нашёл исходную ошибку в обёртке")
	}
}

func TestMaxMigrationVersion(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  uint
	}{
		{
			name: "берёт максимум по .up.sql, игнорируя .down.sql",
			names: []string{
				"0001_init.up.sql", "0001_init.down.sql",
				"0019_usage_dropped.up.sql", "0019_usage_dropped.down.sql",
				"0020_event_quota_default.up.sql", "0020_event_quota_default.down.sql",
			},
			want: 20,
		},
		{
			name:  "порядок в списке не важен",
			names: []string{"0007_x.up.sql", "0002_y.up.sql", "0011_z.up.sql"},
			want:  11,
		},
		{
			name:  "файлы без ведущих цифр игнорируются",
			names: []string{"readme.txt", "notes.up.sql", "0003_ok.up.sql"},
			want:  3,
		},
		{
			name:  "пустой список — 0",
			names: nil,
			want:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxMigrationVersion(tc.names); got != tc.want {
				t.Errorf("maxMigrationVersion(%v) = %d, want %d", tc.names, got, tc.want)
			}
		})
	}
}

// maxEmbeddedPGVersion считает по реальному embed FS и должен вернуть номер
// последней встроенной миграции. Растёт с добавлением миграций — проверяем
// нижнюю границу (>= 19, последняя закоммиченная на момент RA-8), а не точное
// число, чтобы не ломаться при добавлении новых миграций.
func TestMaxEmbeddedPGVersion(t *testing.T) {
	got, err := maxEmbeddedPGVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedPGVersion: %v", err)
	}
	if got < 19 {
		t.Errorf("maxEmbeddedPGVersion = %d, want >= 19", got)
	}
}

// TestSchemaGateErr закрепляет чистую логику version-гейта схемы: got==want —
// ок (nil); got<want — отставание; got>want — база впереди встроенной версии
// (даунгрейд бинаря, audit3); dirty перекрывает всё. label подставляется в текст.
func TestSchemaGateErr(t *testing.T) {
	cases := []struct {
		name       string
		label      string
		got        uint
		dirty      bool
		want       uint
		wantErr    bool
		wantSubstr []string
	}{
		{
			name: "равные версии — ок",
			got:  20, want: 20, wantErr: false,
		},
		{
			name: "отставание", label: "PG",
			got: 18, want: 20, wantErr: true,
			wantSubstr: []string{"PG", "18", "20", "отстаёт"},
		},
		{
			name: "база впереди встроенной — обновите бинарь", label: "PG",
			got: 22, want: 20, wantErr: true,
			wantSubstr: []string{"PG", "22", "20", "впереди", "бинарь"},
		},
		{
			name: "dirty перекрывает совпадение версий", label: "ClickHouse",
			got: 20, dirty: true, want: 20, wantErr: true,
			wantSubstr: []string{"ClickHouse", "dirty", "20"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := schemaGateErr(tc.label, tc.got, tc.dirty, tc.want)
			if tc.wantErr && err == nil {
				t.Fatalf("schemaGateErr(%q,%d,%v,%d) = nil, want error", tc.label, tc.got, tc.dirty, tc.want)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("schemaGateErr(%q,%d,%v,%d) = %v, want nil", tc.label, tc.got, tc.dirty, tc.want, err)
			}
			if err == nil {
				return
			}
			msg := err.Error()
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(msg, sub) {
					t.Errorf("сообщение %q не содержит %q", msg, sub)
				}
			}
		})
	}
}

// TestMaxEmbeddedCHVersion: максимум встроенных CH-миграций считается по embed FS
// и не меньше 11 (последняя закоммиченная на момент audit3), растёт с новыми.
func TestMaxEmbeddedCHVersion(t *testing.T) {
	got, err := maxEmbeddedCHVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedCHVersion: %v", err)
	}
	if got < 11 {
		t.Errorf("maxEmbeddedCHVersion = %d, want >= 11", got)
	}
}

func TestNeedsRetention(t *testing.T) {
	ddl := "CREATE TABLE events (...) TTL toDateTime(timestamp) + toIntervalDay(90)"
	if needsRetention(ddl, 90) {
		t.Error("same TTL: want no change")
	}
	if !needsRetention(ddl, 180) {
		t.Error("different TTL: want change")
	}
}
