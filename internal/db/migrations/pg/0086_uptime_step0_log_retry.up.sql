-- backward-compatible: yes (ADD COLUMN nullable, без дефолта)
--
-- У uptime Detector отправляет "down" ровно один раз при успехе (в отличие от
-- SendStepIfDue пяти других источников) — ретраить нужно строго лог, не уведомление.
-- notify_open_channels — снимок каналов, которым "down" реально ушёл (пишется один раз
-- сразу после доставки); NULL — ретраить нечего (лог завершён либо инцидент старше миграции).
ALTER TABLE incidents ADD COLUMN notify_open_channels bigint[];
