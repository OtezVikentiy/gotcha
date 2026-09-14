-- backward-compatible: yes (UPDATE только числовой колонки, схема не меняется)
-- Store.Create отклоняет burn_short_minutes строго между 0 и 5 для
-- availability/latency (ErrBurnShortMinTooSmall; 0 значит «умолчание», не
-- отклоняется), но Store не даёт Update — строки старше проверки обновить
-- было нечем. То же условие, что при создании: 0 не трогаем, чтобы не
-- заморозить «умолчание» в текущее числовое значение минимума.
UPDATE slos SET burn_short_minutes = 5
    WHERE sli_kind IN ('availability', 'latency')
    AND burn_short_minutes > 0 AND burn_short_minutes < 5;
