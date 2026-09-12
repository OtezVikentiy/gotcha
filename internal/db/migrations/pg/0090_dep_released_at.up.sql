-- backward-compatible: yes (аддитивно — nullable-колонки без DEFAULT)
--
-- Без отметки момента снятия подавления часы лесенки (elapsed = now - StartedAt) считали бы
-- весь простой под подавлением как «задержка ступени настала» — ребёнок получил бы все
-- просроченные ступени каскадом. Тот же приём, что incident_groups.resolved_at: перезапуск
-- часов от освобождения. NULL — подавления не было или инцидент ещё под ним.
ALTER TABLE host_incidents ADD COLUMN dep_released_at timestamptz;
ALTER TABLE incidents      ADD COLUMN dep_released_at timestamptz;
