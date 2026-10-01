ALTER TABLE assets ADD COLUMN component TEXT NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN component_version TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_assets_component ON assets(repository_id, component, component_version, id);
