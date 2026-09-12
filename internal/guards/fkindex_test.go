package guards

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Отдельно от fkIndexQuery: пустой список непокрытых неотличим от «схема не
// поднялась» — нужен независимый счётчик всех ограничений.
const fkConstraintCountQuery = `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`

// Не проверяет тип доступа индекса (access method) — GIN/GiST/BRIN не отличит
// от btree и молча засчитает покрытием, появись такой индекс на FK-колонке.
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

var fkIndexExemptions = []Exemption{}

const maxFKIndexExemptions = 0

// Опрашивает живой каталог СУБД, не сканирует Go-исходники — новый внешний
// ключ без индекса уронит сборку в момент, когда миграция его заводит.
func TestForeignKeysHaveCoveringIndex(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var total int
	if err := pool.QueryRow(ctx, fkConstraintCountQuery).Scan(&total); err != nil {
		t.Fatalf("не удалось посчитать ограничения внешнего ключа в каталоге: %v", err)
	}
	// Пустой список — это не «нарушений нет», а потенциально «схема не поднялась».
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
