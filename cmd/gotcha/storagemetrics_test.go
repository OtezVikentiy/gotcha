package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// okSource — diskSource-дубль, всегда успешно отдающий фиксированные free/total.
type okSource struct{ free, total int64 }

func (okSource) storeLabel() string { return "ok" }
func (s okSource) stat(context.Context) (free, total uint64, err error) {
	return uint64(s.free), uint64(s.total), nil
}

// failSource — diskSource-дубль, всегда отдающий ошибку чтения.
type failSource struct{ err error }

func (failSource) storeLabel() string { return "fail" }
func (s failSource) stat(context.Context) (free, total uint64, err error) {
	return 0, 0, s.err
}

// TestStorageMetricsArePublished: заполнение диска не было видно ниоткуда —
// девяносто пять процентов и сто выглядели одинаково, и до отказа не было ни
// одного сигнала.
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

// TestStorageMetricsSurviveOneStorageFailing: сбор показателей не должен
// становиться новым источником отказа — ошибка чтения одного хранилища не
// убирает метрики другого и не роняет выдачу целиком (панику здесь ловит сам
// testing.T: необработанная паника в этом же горутине завалит тест).
func TestStorageMetricsSurviveOneStorageFailing(t *testing.T) {
	var reg selfmetrics.Registry
	registerStorageMetrics(&reg,
		failSource{err: errors.New("disk enumeration failed")},
		okSource{free: 5 << 30, total: 50 << 30})

	out := reg.Gather()
	if !strings.Contains(out, `store="ok"`) {
		t.Fatal("метрики рабочего источника пропали из выдачи из-за отказа соседнего")
	}
	// Значение живого источника — настоящее число, не NaN: приоритетный
	// опрос успел отработать синхронно внутри registerStorageMetrics.
	// Gather форматирует через strconv.FormatFloat(..., 'g', -1, 64) — большое
	// число (50<<30 байт) выходит в экспоненциальной записи, не десятичной.
	if !strings.Contains(out, "gotcha_storage_total_bytes{store=\"ok\"} 5.36870912e+10") {
		t.Fatalf("рабочий источник должен показать реальный total_bytes, выдача:\n%s", out)
	}
	// Упавший источник не паникует и не портит остальную выдачу — его строки
	// остаются в тексте (NaN, а не 0: см. docstring diskPoller.snap про то,
	// почему 0 здесь опаснее отсутствующего значения), просто без числа.
	if !strings.Contains(out, `store="fail"`) {
		t.Fatal("упавший источник должен остаться в выдаче как NaN, а не исчезнуть молча")
	}
	if !strings.Contains(out, "gotcha_storage_free_bytes{store=\"fail\"} NaN") {
		t.Fatalf("до первого успешного опроса значение обязано быть NaN, а не 0 (0 читался бы как «диск полон»), выдача:\n%s", out)
	}
}

// TestStorageMetricsUsedBytesIsHonestlyNamed: PostgreSQL не может честно
// сообщить свободное/общее место на томе через открытое соединение (см.
// docstring pgUsedBytesSource и отчёт задачи) — вместо подмены смысла
// свободного места занятым публикуется отдельная метрика с другим именем.
func TestStorageMetricsUsedBytesIsHonestlyNamed(t *testing.T) {
	var reg selfmetrics.Registry
	registerUsedBytesMetric(&reg, "postgres", okUsedSource{used: 42})

	out := reg.Gather()
	if !strings.Contains(out, `gotcha_storage_used_bytes{store="postgres"} 42`) {
		t.Fatalf("used_bytes должен опубликовать реальное значение под меткой store, выдача:\n%s", out)
	}
	// Проверяем именно заголовок семейства метрики (# TYPE ...), а не
	// произвольное вхождение подстроки: HELP-текст used_bytes сознательно
	// упоминает имена free_bytes/total_bytes как отсылку к соседней метрике
	// (см. registerUsedBytesMetric), и наивный Contains споткнулся бы об
	// собственный же комментарий.
	if strings.Contains(out, "\n# TYPE gotcha_storage_free_bytes ") ||
		strings.Contains(out, "\n# TYPE gotcha_storage_total_bytes ") {
		t.Fatal("источник, который не может честно отдать free/total, не должен публиковать эти метрики")
	}
}

type okUsedSource struct{ used uint64 }

func (s okUsedSource) stat(context.Context) (uint64, error) { return s.used, nil }

// TestStorageSourcesAgainstRealDatabases гоняет РЕАЛЬНЫЕ chDiskSource.stat/
// pgUsedBytesSource.stat против контейнеров testenv, а не заглушек: шаг 1
// задачи был именно про то, какие величины физически доступны по системной
// таблице ClickHouse и функции PostgreSQL — заглушка okSource/okUsedSource
// этого не проверяет, только форму интерфейса. Миграции здесь не нужны:
// system.disks и pg_database_size не зависят от схемы, но testenv.Migrated*
// проще самостоятельной сборки DSN, а лишний DDL этому запросу не мешает.
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
	// Не просто «без ошибки»: свежий контейнер testenv гарантированно имеет
	// физический том под данными, total_space обязан быть положительным.
	// free <= total — иначе это не место на диске, а что-то другое.
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
	// Свежесмигрированная база уже не пуста (таблицы, индексы) — 0 значил бы,
	// что запрос ушёл не в ту базу или вернул не то поле.
	if used == 0 {
		t.Fatal("postgres: pg_database_size вернул 0 на свежемигрированной базе")
	}
}
