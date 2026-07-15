ALTER TABLE transactions ADD COLUMN IF NOT EXISTS measurements Map(String, Float64)
