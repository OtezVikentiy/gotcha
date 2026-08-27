-- backward-compatible: yes (новый индекс)
-- export_jobs_created_by_fkey (0081) — без покрывающего индекса: удаление
-- пользователя (ON DELETE CASCADE) искало бы его заявки последовательным
-- сканом export_jobs. created_by NOT NULL — индекс не частичный, тот же
-- паттерн, что sessions_user_id_idx (0002) для точно такого же FK-профиля
-- (NOT NULL REFERENCES users(id) ON DELETE CASCADE).
CREATE INDEX export_jobs_created_by_idx ON export_jobs (created_by);
