-- Откат снимает ограничение, но НЕ восстанавливает NULL взамен нулей,
-- проставленных вверх-миграцией: они были страховкой на случай дефектных
-- строк, а не данными, которые нужно вернуть.
ALTER TABLE host_incidents ALTER COLUMN current_value DROP NOT NULL;
ALTER TABLE host_incidents ALTER COLUMN peak_value DROP NOT NULL;
