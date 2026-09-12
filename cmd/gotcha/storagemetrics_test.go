package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type okSource struct{ free, total int64 }

func (okSource) storeLabel() string { return "ok" }
func (s okSource) stat(context.Context) (free, total uint64, err error) {
	return uint64(s.free), uint64(s.total), nil
}

type failSource struct{ err error }

func (failSource) storeLabel() string { return "fail" }
func (s failSource) stat(context.Context) (free, total uint64, err error) {
	return 0, 0, s.err
}

func TestStorageMetricsArePublished(t *testing.T) {
	var reg selfmetrics.Registry
	registerStorageMetrics(&reg, okSource{free: 1 << 30, total: 20 << 30})

	out := reg.Gather()
	for _, want := range []string{"gotcha_storage_free_bytes", "gotcha_storage_total_bytes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выдаче нет %q: заполнение диска по-прежнему не видно ниоткуда", want)
		}
	}
	if strings.Contains(out, "gotcha_storage_total_bytes 0") {
		t.Fatal("общий объём нулевой — метрика есть, но смысла не несёт")
	}
}

func TestStorageMetricsSurviveOneStorageFailing(t *testing.T) {
	var reg selfmetrics.Registry
	registerStorageMetrics(&reg,
		failSource{err: errors.New("disk enumeration failed")},
		okSource{free: 5 << 30, total: 50 << 30})

	out := reg.Gather()
	if !strings.Contains(out, `store="ok"`) {
		t.Fatal("метрики рабочего источника пропали из выдачи из-за отказа соседнего")
	}
	// Большое число (50<<30 байт) Gather форматирует в экспоненциальной записи,
	// не десятичной.
	if !strings.Contains(out, "gotcha_storage_total_bytes{store=\"ok\"} 5.36870912e+10") {
		t.Fatalf("рабочий источник должен показать реальный total_bytes, выдача:\n%s", out)
	}
	if !strings.Contains(out, `store="fail"`) {
		t.Fatal("упавший источник должен остаться в выдаче как NaN, а не исчезнуть молча")
	}
	if !strings.Contains(out, "gotcha_storage_free_bytes{store=\"fail\"} NaN") {
		t.Fatalf("до первого успешного опроса значение обязано быть NaN, а не 0 (0 читался бы как «диск полон»), выдача:\n%s", out)
	}
}

func TestStorageMetricsUsedBytesIsHonestlyNamed(t *testing.T) {
	var reg selfmetrics.Registry
	registerUsedBytesMetric(&reg, "postgres", okUsedSource{used: 42})

	out := reg.Gather()
	if !strings.Contains(out, `gotcha_storage_used_bytes{store="postgres"} 42`) {
		t.Fatalf("used_bytes должен опубликовать реальное значение под меткой store, выдача:\n%s", out)
	}
	// Заголовок семейства (# TYPE ...), не произвольная подстрока: HELP-текст
	// used_bytes сам упоминает имена free_bytes/total_bytes.
	if strings.Contains(out, "\n# TYPE gotcha_storage_free_bytes ") ||
		strings.Contains(out, "\n# TYPE gotcha_storage_total_bytes ") {
		t.Fatal("источник, который не может честно отдать free/total, не должен публиковать эти метрики")
	}
}

type okUsedSource struct{ used uint64 }

func (s okUsedSource) stat(context.Context) (uint64, error) { return s.used, nil }

// Миграции здесь не нужны (system.disks и pg_database_size не зависят от
// схемы), но testenv.Migrated* проще самостоятельной сборки DSN.
func TestStorageSourcesAgainstRealDatabases(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres/clickhouse containers")
	}
	ctx := context.Background()

	ch := testenv.MigratedCH(t)
	chFree, chTotal, err := (chDiskSource{conn: ch}).stat(ctx)
	if err != nil {
		t.Fatalf("clickhouse system.disks: %v", err)
	}
	if chTotal == 0 {
		t.Fatal("clickhouse: total_space == 0 — system.disks не отдал реальный том")
	}
	if chFree > chTotal {
		t.Fatalf("clickhouse: free_space (%d) > total_space (%d) — не похоже на настоящий диск", chFree, chTotal)
	}

	pg := testenv.MigratedPG(t)
	used, err := (pgUsedBytesSource{pool: pg}).stat(ctx)
	if err != nil {
		t.Fatalf("postgres pg_database_size: %v", err)
	}
	if used == 0 {
		t.Fatal("postgres: pg_database_size вернул 0 на свежемигрированной базе")
	}
}

func TestExportDirUsedBytesSourceReflectsRealFiles(t *testing.T) {
	dir := t.TempDir()
	src := exportDirUsedBytesSource{dir: dir}

	used, err := src.stat(context.Background())
	if err != nil {
		t.Fatalf("stat на пустом каталоге: %v", err)
	}
	if used != 0 {
		t.Errorf("used = %d на пустом каталоге, want 0", used)
	}

	if err := os.WriteFile(filepath.Join(dir, "1.csv"), make([]byte, 500), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.csv.part"), make([]byte, 250), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	used, err = src.stat(context.Background())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if used != 750 {
		t.Errorf("used = %d, want 750 (500 + 250 — сумма файлов каталога)", used)
	}
}

func TestExportDirUsedBytesSourceStatMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	src := exportDirUsedBytesSource{dir: dir}

	if _, err := src.stat(context.Background()); err == nil {
		t.Fatal("stat на отсутствующем каталоге = nil, want ошибку")
	}
}

func TestExportDirUsedBytesSourceStatUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root игнорирует биты доступа — проба не сработает под root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("Chmod(%q, 0o000): %v", dir, err)
	}
	defer os.Chmod(dir, 0o755) // иначе t.TempDir() не сможет убрать за собой

	src := exportDirUsedBytesSource{dir: dir}
	if _, err := src.stat(context.Background()); err == nil {
		t.Fatal("stat на недоступном каталоге = nil, want ошибку")
	}
}
