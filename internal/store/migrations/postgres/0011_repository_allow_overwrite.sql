ALTER TABLE repositories ADD COLUMN allow_overwrite BOOLEAN NOT NULL DEFAULT TRUE;
UPDATE repositories SET allow_overwrite = FALSE WHERE type = 'hosted' AND format IN ('npm', 'cargo', 'pypi');
