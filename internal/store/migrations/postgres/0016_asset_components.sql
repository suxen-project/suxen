ALTER TABLE assets ADD COLUMN component TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN component_version TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN retention_group TEXT COLLATE "C" NOT NULL DEFAULT '';
CREATE INDEX idx_assets_component ON assets(repository_id, component, component_version, id);
CREATE INDEX idx_assets_retention_group ON assets(repository_id, retention_group, id);
