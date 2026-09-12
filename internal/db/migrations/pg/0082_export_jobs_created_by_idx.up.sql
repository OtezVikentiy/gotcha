-- backward-compatible: yes (новый индекс)
-- Покрывает export_jobs_created_by_fkey — без индекса ON DELETE CASCADE искал бы
-- заявки пользователя последовательным сканом.
CREATE INDEX export_jobs_created_by_idx ON export_jobs (created_by);
