-- backward-compatible: yes (аддитивно — nullable-колонки без DEFAULT)
--
-- Аудит перед 1.0, находка K1-4 (DEDUP.md, кластер часов лесенки): инцидент,
-- подавленный зависимостью (suppressed_by_dep, миграция 0078), при снятии
-- подавления возобновляет эскалацию с той же escalation_level/StartedAt, на
-- которых был подавлен — а не с нуля. Без отдельной отметки момента снятия
-- «часы лесенки» (elapsed = now - StartedAt, где StartedAt уже раз
-- перезапускается от COALESCE(group.resolved_at, started_at), см. 0067)
-- считали бы весь простой под подавлением как «задержка ступени настала»,
-- и ребёнок, просидевший под упавшим родителем час, получил бы все
-- просроченные ступени лесенки каскадом — по одной ступени за тик, залпом.
--
-- dep_released_at — тот же приём, что уже применён к выходу из группы
-- инцидентов (incident_groups.resolved_at): перезапуск часов от события
-- освобождения, не от исходного открытия. NULL — подавления не было или
-- инцидент ещё под ним; писатели — host.IncidentService.ClearSuppressed и
-- uptime.Service.ClearSuppressedByDep (задача 1 волны 1).
ALTER TABLE host_incidents ADD COLUMN dep_released_at timestamptz;
ALTER TABLE incidents      ADD COLUMN dep_released_at timestamptz;
