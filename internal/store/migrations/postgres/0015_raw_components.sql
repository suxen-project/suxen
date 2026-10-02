ALTER TABLE cleanup_policies ADD COLUMN retention_order TEXT NOT NULL DEFAULT 'updatedAt';
ALTER TABLE assets ALTER COLUMN path TYPE TEXT COLLATE "C";
ALTER TABLE assets ADD COLUMN component TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN component_version TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN component_version_key TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN retention_group TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN derived_revision TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_assets_component_versions ON assets(repository_id, component, component_version_key DESC, component_version DESC);
CREATE INDEX idx_assets_retention_group ON assets(repository_id, retention_group, id);
