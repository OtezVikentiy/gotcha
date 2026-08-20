-- backward-compatible: yes (аддитивно — новая таблица + колонки с DEFAULT)
CREATE TABLE alert_dependencies (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id        bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    parent_host_id    bigint REFERENCES hosts(id)    ON DELETE CASCADE,
    parent_monitor_id bigint REFERENCES monitors(id) ON DELETE CASCADE,
    child_host_id     bigint REFERENCES hosts(id)    ON DELETE CASCADE,
    child_monitor_id  bigint REFERENCES monitors(id) ON DELETE CASCADE,
    child_label_scope text CHECK (child_label_scope IN ('env','role')),
    child_label_value text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alert_dependencies_one_parent
        CHECK ((parent_host_id IS NOT NULL) <> (parent_monitor_id IS NOT NULL)),
    CONSTRAINT alert_dependencies_one_child
        CHECK ( (child_host_id IS NOT NULL)::int
              + (child_monitor_id IS NOT NULL)::int
              + (child_label_scope IS NOT NULL AND child_label_value IS NOT NULL)::int = 1 ),
    CONSTRAINT alert_dependencies_label_pair
        CHECK ((child_label_scope IS NULL) = (child_label_value IS NULL))
);
CREATE INDEX alert_dependencies_project_id_idx     ON alert_dependencies (project_id);
CREATE INDEX alert_dependencies_parent_host_idx    ON alert_dependencies (parent_host_id);
CREATE INDEX alert_dependencies_parent_monitor_idx ON alert_dependencies (parent_monitor_id);
CREATE INDEX alert_dependencies_child_host_idx     ON alert_dependencies (child_host_id);
CREATE INDEX alert_dependencies_child_monitor_idx  ON alert_dependencies (child_monitor_id);

ALTER TABLE host_incidents ADD COLUMN suppressed_by_dep boolean NOT NULL DEFAULT false;
ALTER TABLE incidents      ADD COLUMN suppressed_by_dep boolean NOT NULL DEFAULT false;
