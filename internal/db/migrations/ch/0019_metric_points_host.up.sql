-- backward-compatible: yes
ALTER TABLE metric_points ADD COLUMN IF NOT EXISTS host LowCardinality(String) DEFAULT '';
