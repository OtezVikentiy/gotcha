-- Отдельный счётчик потребления транзакций: транзакции не должны съедать
-- бюджет ошибок (events_count), и наоборот. Колонка в той же строке org_usage,
-- но инкрементируется независимо (см. org.IncTransactionUsage).
ALTER TABLE org_usage ADD COLUMN transactions_count bigint NOT NULL DEFAULT 0;
