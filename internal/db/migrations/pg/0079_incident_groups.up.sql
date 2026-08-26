-- backward-compatible: yes (аддитивно — новая таблица + NULL-колонки group_id + частичные индексы)
CREATE TABLE incident_groups (
    id            BIGSERIAL PRIMARY KEY,
    project_id    BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    root_source   TEXT   NOT NULL CHECK (root_source IN ('host','uptime')),
    root_incident_id BIGINT NOT NULL,
    root_node_kind TEXT  NOT NULL CHECK (root_node_kind IN ('host','monitor')),
    root_node_id  BIGINT NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    UNIQUE (root_source, root_incident_id)
);
CREATE INDEX incident_groups_open_idx ON incident_groups(project_id) WHERE resolved_at IS NULL;
-- Покрывающий индекс под FK project_id: сторож TestForeignKeysHaveCoveringIndex не
-- засчитывает частичный incident_groups_open_idx (предикат не «IS NOT NULL» по колонке FK),
-- а каскадное удаление проекта иначе шло бы последовательным сканом (прецедент — 0078).
CREATE INDEX incident_groups_project_id_idx ON incident_groups(project_id);

ALTER TABLE host_incidents   ADD COLUMN group_id BIGINT;
ALTER TABLE incidents        ADD COLUMN group_id BIGINT;
ALTER TABLE metric_incidents ADD COLUMN group_id BIGINT;
ALTER TABLE slo_incidents    ADD COLUMN group_id BIGINT;
CREATE INDEX host_incidents_group_idx   ON host_incidents(group_id)   WHERE group_id IS NOT NULL;
CREATE INDEX incidents_group_idx        ON incidents(group_id)        WHERE group_id IS NOT NULL;
CREATE INDEX metric_incidents_group_idx ON metric_incidents(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX slo_incidents_group_idx    ON slo_incidents(group_id)    WHERE group_id IS NOT NULL;
