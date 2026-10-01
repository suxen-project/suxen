ALTER TABLE cleanup_policies ADD COLUMN retention_order TEXT NOT NULL DEFAULT 'updatedAt';
