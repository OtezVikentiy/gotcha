package guards

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fkConstraintCountQuery — сколько всего ограничений внешнего ключа есть в
// каталоге PostgreSQL. Отдельно от fkIndexQuery ниже: та отдаёт только
// НЕПОКРЫТЫЕ ограничения, и на здоровой схеме её результат — пустой список.
// Пустой список непокрытых неотличим от «схема не поднялась, каталог пуст» —
// именно поэтому нужен независимый счётчик ВСЕХ ограничений (см. защиту от
// слепоты в TestForeignKeysHaveCoveringIndex).
const fkConstraintCountQuery = `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`

// fkIndexQuery — запрос к системному каталогу, отдающий ограничения внешнего
// ключа, для которых нет индекса, начинающегося со ссылающейся колонки (для
// составных — с той же последовательности колонок в том же порядке).
//
// Текст ПЕРЕНЕСЁН БУКВА В БУКВУ из задачи 2 этого же подпроекта
// (.superpowers/sdd/2026-08-02-audit-query-cost-plan/task-2-report.md,
// раздел «Раунд правок 1» → «Итоговый запрос (передаётся задаче 4 как
// есть)») — не переписан заново. Причина: первая версия запроса
// (написанная и отлаженная в задаче 2 до ревью) имела два критических
// дефекта, оба нашло ревью и оба воспроизведены на изолированной схеме
// шестью случаями:
//
//  1. `indpred IS NULL` как единственное условие отбрасывало ЛЮБОЙ частичный
//     индекс — не отличая индекс с посторонним предикатом (не покрывает
//     каскад) от индекса с предикатом «ссылающаяся колонка не пуста»
//     (`check_queue.leased_by`, `issues.assignee_id`, заведённые той же
//     задачей 2) — на них сторож с наивным запросом был бы красным на
//     собственном результате подпроекта.
//  2. Запрос не проверял `indisvalid` — слеп ровно к ловушке из докблока
//     миграции 0031 и upgrade.md: сорвавшееся `CREATE INDEX CONCURRENTLY`
//     оставляет в каталоге недействительный индекс, которым планировщик не
//     пользуется, а наивный запрос засчитал бы его покрытием.
//
// Обе правки в этом тексте уже есть (`i.indisvalid`, разбор indpred через
// pg_get_expr на «IS NOT NULL по каждой колонке и ничего больше»). Переписывать
// эту логику заново значило бы рисковать заново наступить на те же два
// дефекта, уже один раз найденные и доказанные ревью — поэтому текст здесь
// ровно тот, что передан задаче 4 «как есть».
//
// Оговорка, которую попросил перенести ревьюер задачи 2 (там же, раздел
// «Другие свойства индекса, лишающие его пригодности», пункт 1): запрос НЕ
// проверяет ТИП ДОСТУПА индекса (access method). Проверка каскада — обычное
// равенство (fk_col = $1), которое поддерживают btree и hash; GIN/GiST/BRIN/
// SP-GiST в общем случае для простого равенства планировщиком не
// применяются. Сегодня в схеме проекта все индексы — обычные btree (CREATE
// INDEX без USING), риск нулевой, но если когда-нибудь на FK-колонке
// появится экзотический индекс (GIN и т.п.), этот запрос его от полноценного
// btree не отличит и молча засчитает покрытием.
const fkIndexQuery = `
WITH fk AS (
    SELECT
        c.oid AS con_oid,
        c.conrelid,
        c.conname,
        (
            SELECT array_agg(a.attname ORDER BY k.ord)
            FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
            JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
        ) AS fk_columns
    FROM pg_constraint c
    WHERE c.contype = 'f'
),
covering_index AS (
    SELECT DISTINCT fk.con_oid
    FROM fk
    JOIN pg_index i
      ON i.indrelid = fk.conrelid
     AND i.indisvalid
     AND (i.indkey::int2[])[0:cardinality(fk.fk_columns)-1]
         = (
             SELECT array_agg(a.attnum ORDER BY k.ord)
             FROM unnest(fk.fk_columns) WITH ORDINALITY AS k(colname, ord)
             JOIN pg_attribute a ON a.attrelid = fk.conrelid AND a.attname = k.colname
           )::int2[]
    WHERE
        i.indpred IS NULL
        OR (
            -- Частичный индекс засчитывается покрытием, только если его
            -- предикат — это "<колонка> IS NOT NULL" по КАЖДОЙ колонке
            -- ограничения, объединённые через AND, в любом порядке, и
            -- ничего больше (см. «Раунд правок 1» в отчёте задачи 2).
            (
                SELECT bool_and(
                    pg_get_expr(i.indpred, i.indrelid) ~* ('\m' || col || '\M\s+IS\s+NOT\s+NULL')
                )
                FROM unnest(fk.fk_columns) AS col
            )
            AND regexp_replace(
                    regexp_replace(
                        pg_get_expr(i.indpred, i.indrelid),
                        '\(?\m(' || array_to_string(fk.fk_columns, '|') || ')\M\s+IS\s+NOT\s+NULL\)?',
                        '', 'gi'
                    ),
                    '\s*AND\s*|[()]', '', 'gi'
                ) = ''
        )
)
SELECT fk.conrelid::regclass::text AS table_name, fk.conname AS constraint_name,
       fk.fk_columns::text AS fk_columns
FROM fk
WHERE fk.con_oid NOT IN (SELECT con_oid FROM covering_index)
ORDER BY 1, 2;
`

// fkIndexExemptions — внешние ключи без покрывающего индекса, допущенные
// осознанно. Пусто на момент написания: контрольный прогон запроса на
// боевой схеме проекта (после миграций подпроекта, см. task-2-report.md)
// отдал ноль непокрытых ограничений из фактического числа в каталоге —
// исключения заводить не пришлось.
var fkIndexExemptions = []Exemption{}

// maxFKIndexExemptions — потолок долга. Ноль: находка, ради которой писался
// весь подпроект («цена запросов»), — это FK-колонка, участвующая в JOIN/
// поиске без индекса, а не то, что можно оставить «на будущее» без причины —
// добавление исключения обязано поднимать это число осознанной правкой, а не
// проходить молча.
const maxFKIndexExemptions = 0

// TestForeignKeysHaveCoveringIndex: у каждого ограничения внешнего ключа в
// каталоге PostgreSQL обязан быть индекс, покрывающий ссылающуюся
// колонку (для составных ключей — ту же последовательность колонок).
//
// Источник — сама применённая схема (testenv.MigratedPG поднимает свежую
// базу и накатывает все миграции), а не список, переписанный руками в
// тесте: новый внешний ключ без индекса уронит сборку в момент, когда
// миграция его заводит, а не всплывёт годы спустя разбором «почему запрос
// на удаление аккаунта минуты идёт» — ровно то, зачем весь подпроект и
// затевался (находка №… «цена запросов», см. бриф задачи 4).
//
// Правило НЕ построчно сканирует Go-исходники (в отличие от большинства
// соседей пакета guards) — оно опрашивает живой каталог СУБД, поэтому из
// двух обязательных пунктов чеклиста в докблоке package guards (tree.go)
// НИ ОДИН не применим буквально: здесь нет ни "//"-комментариев для
// вычистки (stripTrailingComment), ни построчного сравнения с паттерном
// кода, которое обманули бы фикстуры/примеры internal/guards — весь ввод
// правила приходит из pg_catalog, а не из tree.GoFiles. Оба пункта прочитаны
// и сознательно признаны неприменимыми к этой конкретной форме правила, а
// не пропущены по недосмотру.
func TestForeignKeysHaveCoveringIndex(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var total int
	if err := pool.QueryRow(ctx, fkConstraintCountQuery).Scan(&total); err != nil {
		t.Fatalf("не удалось посчитать ограничения внешнего ключа в каталоге: %v", err)
	}
	// Пустой список ограничений — это не «нарушений нет», а «схема не
	// поднялась». Без этой проверки сломанный стенд (например, миграции не
	// применились) давал бы зелёный тест, неотличимый от чистой схемы: у
	// fkIndexQuery ниже пустой результат означает и «всё покрыто», и «в
	// каталоге вообще нет ограничений» — этот счётчик их различает.
	if total == 0 {
		t.Fatal("в каталоге нет ни одного ограничения внешнего ключа: схема не применилась")
	}

	rows, err := pool.Query(ctx, fkIndexQuery)
	if err != nil {
		t.Fatalf("запрос к каталогу (fkIndexQuery) упал: %v", err)
	}
	defer rows.Close()

	exempt := ExemptedValues(fkIndexExemptions)
	seen := map[string]bool{}
	var missingCount int
	for rows.Next() {
		var table, constraint, columns string
		if err := rows.Scan(&table, &constraint, &columns); err != nil {
			t.Fatalf("scan строки результата: %v", err)
		}
		key := table + "." + constraint
		seen[key] = true
		missingCount++
		if exempt[key] {
			continue
		}
		t.Errorf("%s: ограничение внешнего ключа %s (колонки %s) не покрыто индексом, начинающимся с этих колонок в этом порядке — "+
			"каскадное удаление/JOIN по нему пойдёт последовательным сканом", table, constraint, columns)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("итерация результата: %v", err)
	}

	CheckExemptions(t, "TestForeignKeysHaveCoveringIndex", fkIndexExemptions, maxFKIndexExemptions, seen)

	t.Logf("проверено ограничений внешнего ключа в каталоге: %d, непокрытых индексом: %d (из них в списке исключений: %d)",
		total, missingCount, len(fkIndexExemptions))
}
