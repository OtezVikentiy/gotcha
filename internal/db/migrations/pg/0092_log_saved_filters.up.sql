-- backward-compatible: yes (новые таблицы)
CREATE TABLE log_saved_filters (
    id             bigserial PRIMARY KEY,
    project_id     bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    owner_user_id  bigint     NULL REFERENCES users(id) ON DELETE CASCADE,
    author_user_id bigint     NULL REFERENCES users(id) ON DELETE SET NULL,
    name           text   NOT NULL,
    payload        jsonb  NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- owner_user_id IS NULL — общий фильтр проекта (не отдельная колонка scope, чтобы не разойтись).
-- Личный фильтр уходит с владельцем (CASCADE), общий переживает автора (SET NULL).

CREATE UNIQUE INDEX log_saved_filters_shared_name_idx
    ON log_saved_filters (project_id, lower(name)) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX log_saved_filters_personal_name_idx
    ON log_saved_filters (project_id, owner_user_id, lower(name)) WHERE owner_user_id IS NOT NULL;

CREATE TABLE log_default_filters (
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filter_id  bigint NOT NULL REFERENCES log_saved_filters(id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, user_id)
);
