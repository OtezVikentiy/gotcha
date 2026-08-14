-- backward-compatible: yes
CREATE TABLE host_threshold_settings (
    project_id bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    disk_enabled     boolean NOT NULL DEFAULT true,
    disk_threshold   double precision NOT NULL DEFAULT 0.90 CHECK (disk_threshold > 0 AND disk_threshold <= 1),
    memory_enabled   boolean NOT NULL DEFAULT true,
    memory_threshold double precision NOT NULL DEFAULT 0.90 CHECK (memory_threshold > 0 AND memory_threshold <= 1),
    load_enabled     boolean NOT NULL DEFAULT true,
    load_threshold   double precision NOT NULL DEFAULT 2.0 CHECK (load_threshold > 0),
    silent_enabled   boolean NOT NULL DEFAULT true,
    silent_after_seconds int NOT NULL DEFAULT 300 CHECK (silent_after_seconds >= 180),
    updated_at timestamptz NOT NULL DEFAULT now()
);
