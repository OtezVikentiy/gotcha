package db_test

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migration0030Path — путь к файлу миграции относительно каталога пакета, по
// той же причине, что и migration0029Path в migrate_0029_test.go: go test
// запускает тесты с рабочей директорией внутри internal/db, а go:embed
// пакует те же самые файлы без преобразований.
const migration0030Path = "migrations/pg/0030_regression_values_ms.up.sql"

// TestMigration0030IsMarkedBreaking: 0030 меняет смысл уже записанных значений
// (микросекунды → миллисекунды). Откат восстанавливает прежние числа (с
// точностью двоичной арифметики, см. TestMigrate0030RecomputesDurationValues),
// но прежний код на новых числах покажет длительности заниженными в тысячу
// раз, поэтому миграция несовместима в обе стороны и обязана нести пометку.
//
// Проверка нужна отдельно, потому что destructiveSQL распознаёт формы
// разрушения схемы, а не изменения данных: UPDATE он не считает разрушительным,
// и без этого теста пометка держалась бы только вниманием автора.
//
// Маркер разбирает parseCompatMarker (internal/db/compat.go) — он читает
// ТОЛЬКО первую строку файла (закреплено TestParseCompatMarker в
// compat_internal_test.go, кейс «маркер не в первой строке» даёт ok=false).
// Поэтому здесь, как и в migrate_0029_test.go, сравнивается именно первая
// строка целиком, а не поиск подстроки где угодно в файле: поиск подстроки
// остался бы зелёным, если бы кто-то дописал пояснение перед маркером, а
// embeddedCompat в проде в этом случае вернул бы «миграция без маркера» и
// уронил старт схемы — ровно то, что этот тест обязан ловить.
func TestMigration0030IsMarkedBreaking(t *testing.T) {
	b, err := os.ReadFile(migration0030Path)
	if err != nil {
		t.Fatalf("прочитать миграцию: %v", err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	first = strings.TrimSpace(first)
	if first != "-- backward-compatible: no" {
		t.Fatalf("первая строка %s = %q, want %q", migration0030Path, first, "-- backward-compatible: no")
	}
}

// durationRoundTripTolerance — допуск при сравнении длительности до и после
// пары up/down. duration приходит из quantilesMerge ClickHouse и почти
// никогда не кратен 1000 (см. пример ниже, 643271.4 — правдоподобное сырое
// значение квантиля). Деление на 1000 и обратное умножение в double
// precision не обязаны быть побитово взаимно обратны: расхождение порядка
// 1e-10 на величинах ~1e6 — свойство двоичной арифметики с плавающей
// точкой, а не небрежность миграции или теста. Поэтому раунд-трип
// проверяется с допуском, а не на точное равенство; округлые значения (кратные
// 1000, как исходные duration ниже) им не подвержены и раньше проверялись
// точным сравнением — это не была проверка общего случая.
const durationRoundTripTolerance = 1e-6

func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

// TestMigrate0030RecomputesDurationValues — самая опасная точка задачи: правка
// обязана задеть только metric = 'duration' (записан в микросекундах из-за
// дефекта конвертации в пакетных p95-запросах), а web-vital'ы lcp/fcp/ttfb/inp
// (уже в миллисекундах) и безразмерный cls трогать не должна — деление
// испортило бы верные данные. Проверяем и пересчёт, и то, что он не затрагивает
// посторонние строки, а также откат — для кратных 1000 значений точно, для
// нецелых (реалистичных) — с допуском durationRoundTripTolerance.
func TestMigrate0030RecomputesDurationValues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate to 29: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	_, projID := seedProject(t, ctx, pool)

	// duration: записан в микросекундах дефектом пакетных p95-запросов.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'endpoint_p95', 'GET /orders', 'duration', 150000, 300000, 200000)`, projID)
	// duration некратный 1000 — реалистичное сырое значение квантиля
	// ClickHouse (quantilesMerge), проверяет раунд-трип с допуском, а не
	// только удобный случай, где деление/умножение точны.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'endpoint_p95', 'GET /search', 'duration', 643271.4, 910000.7, 712345.6)`, projID)
	// lcp: web-vital, уже в миллисекундах — не должен делиться.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'webvital_p75', '/checkout', 'lcp', 2500, 4000, 3000)`, projID)
	// cls: безразмерный — не должен делиться.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'webvital_p75', '/checkout', 'cls', 0.1, 0.25, 0.15)`, projID)

	if err := db.MigratePGTo(dsn, 30); err != nil {
		t.Fatalf("migrate to 30: %v", err)
	}

	var durBase, durPeak, durCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /orders' AND project_id = $1",
		projID).Scan(&durBase, &durPeak, &durCur); err != nil {
		t.Fatalf("select duration row: %v", err)
	}
	if durBase != 150 || durPeak != 300 || durCur != 200 {
		t.Fatalf("duration после 0030 = (%v,%v,%v), want (150,300,200): пересчёт микросекунд в миллисекунды не сработал",
			durBase, durPeak, durCur)
	}

	var rawBase, rawPeak, rawCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /search' AND project_id = $1",
		projID).Scan(&rawBase, &rawPeak, &rawCur); err != nil {
		t.Fatalf("select non-round duration row: %v", err)
	}
	if !approxEqual(rawBase, 643.2714, durationRoundTripTolerance) ||
		!approxEqual(rawPeak, 910.0007, durationRoundTripTolerance) ||
		!approxEqual(rawCur, 712.3456, durationRoundTripTolerance) {
		t.Fatalf("некратный duration после 0030 = (%v,%v,%v), want ~(643.2714,910.0007,712.3456)",
			rawBase, rawPeak, rawCur)
	}

	var lcpBase, lcpPeak, lcpCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'lcp' AND project_id = $1",
		projID).Scan(&lcpBase, &lcpPeak, &lcpCur); err != nil {
		t.Fatalf("select lcp row: %v", err)
	}
	if lcpBase != 2500 || lcpPeak != 4000 || lcpCur != 3000 {
		t.Fatalf("lcp после 0030 = (%v,%v,%v), want (2500,4000,3000): web-vital затронут ошибочно",
			lcpBase, lcpPeak, lcpCur)
	}

	var clsBase, clsPeak, clsCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'cls' AND project_id = $1",
		projID).Scan(&clsBase, &clsPeak, &clsCur); err != nil {
		t.Fatalf("select cls row: %v", err)
	}
	if clsBase != 0.1 || clsPeak != 0.25 || clsCur != 0.15 {
		t.Fatalf("cls после 0030 = (%v,%v,%v), want (0.1,0.25,0.15): безразмерная метрика затронута ошибочно",
			clsBase, clsPeak, clsCur)
	}

	// Откат: кратные 1000 значения восстанавливаются точно, некратные — с
	// допуском durationRoundTripTolerance (см. комментарий у константы).
	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate down to 29: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /orders' AND project_id = $1",
		projID).Scan(&durBase, &durPeak, &durCur); err != nil {
		t.Fatalf("select duration row after down: %v", err)
	}
	if durBase != 150000 || durPeak != 300000 || durCur != 200000 {
		t.Fatalf("duration после отката = (%v,%v,%v), want (150000,300000,200000)", durBase, durPeak, durCur)
	}

	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /search' AND project_id = $1",
		projID).Scan(&rawBase, &rawPeak, &rawCur); err != nil {
		t.Fatalf("select non-round duration row after down: %v", err)
	}
	if !approxEqual(rawBase, 643271.4, durationRoundTripTolerance*1000) ||
		!approxEqual(rawPeak, 910000.7, durationRoundTripTolerance*1000) ||
		!approxEqual(rawCur, 712345.6, durationRoundTripTolerance*1000) {
		t.Fatalf("некратный duration после отката = (%v,%v,%v), want ~(643271.4,910000.7,712345.6): "+
			"откат — двоичная арифметика, а не точное равенство",
			rawBase, rawPeak, rawCur)
	}

	// lcp/cls после отката должны остаться теми же, какими были записаны — они
	// единственный верный источник, второго нет. Сейчас down-миграция несёт то
	// же WHERE metric = 'duration', что и up, поэтому это ожидаемо проходит;
	// но проверка нужна отдельно, а не «по построению», чтобы будущая правка
	// down-миграции, потерявшая условие по метрике, была поймана здесь, а не
	// молча умножила web-vital'ы и cls на тысячу без возможности отличить
	// испорченные значения от настоящих.
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'lcp' AND project_id = $1",
		projID).Scan(&lcpBase, &lcpPeak, &lcpCur); err != nil {
		t.Fatalf("select lcp row after down: %v", err)
	}
	if lcpBase != 2500 || lcpPeak != 4000 || lcpCur != 3000 {
		t.Fatalf("lcp после отката = (%v,%v,%v), want (2500,4000,3000): "+
			"down-миграция потеряла условие по метрике и задела web-vital",
			lcpBase, lcpPeak, lcpCur)
	}

	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'cls' AND project_id = $1",
		projID).Scan(&clsBase, &clsPeak, &clsCur); err != nil {
		t.Fatalf("select cls row after down: %v", err)
	}
	if clsBase != 0.1 || clsPeak != 0.25 || clsCur != 0.15 {
		t.Fatalf("cls после отката = (%v,%v,%v), want (0.1,0.25,0.15): "+
			"down-миграция потеряла условие по метрике и задела безразмерную метрику",
			clsBase, clsPeak, clsCur)
	}
}
