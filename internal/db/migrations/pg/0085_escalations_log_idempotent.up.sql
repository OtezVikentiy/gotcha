-- backward-compatible: yes (DELETE схлопывает дубли перед UNIQUE — не
-- удаляет ни одной "оригинальной" строки, только дублирующие копии, и не
-- меняет ни одну колонку существующей схемы; новая таблица — аддитивна).
--
-- LogStep и bump escalation_level — два независимых обращения к БД; смерть процесса между
-- ними давала дубль пейджа или дыру в логе. Транзакция бы не помогла: между ними стоит
-- notifyStep — эффект во внешнем outbox, который откатить нельзя. UNIQUE ниже + идемпотентный
-- LogStep держат лог и уровень согласованными без завязки на транзакцию БД.

-- Схлопывает дубли до ALTER TABLE ADD CONSTRAINT (иначе падает на живых данных): на
-- четвёрку (source, incident, channel, step) остаётся строка с минимальным id, лишние
-- удаляются необратимо — обе описывают одну и ту же отправку, различаются только sent_at.
DELETE FROM incident_escalations dup
    USING incident_escalations keep
    WHERE dup.id > keep.id
      AND dup.incident_source = keep.incident_source
      AND dup.incident_id = keep.incident_id
      AND dup.channel_id = keep.channel_id
      AND dup.step = keep.step;

ALTER TABLE incident_escalations
    ADD CONSTRAINT incident_escalations_source_incident_channel_step_key
    UNIQUE (incident_source, incident_id, channel_id, step);

-- Граница попыток на "залогировать эту ступень": без неё устойчиво падающий LogStep не даёт
-- bump'у пройти — следующий тик шлёт ту же ступень заново, пейджинг-шторм на каждом тике.
-- После maxLogFailureAttempts bump продвигается принудительно, с громким логом.
-- Без FK на incident_id — как у incident_escalations: источников 6, таблица одна.
CREATE TABLE escalation_step_log_failures (
    incident_source text NOT NULL,
    incident_id bigint NOT NULL,
    step int NOT NULL,
    attempts int NOT NULL DEFAULT 0,
    last_attempt_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (incident_source, incident_id, step)
);
