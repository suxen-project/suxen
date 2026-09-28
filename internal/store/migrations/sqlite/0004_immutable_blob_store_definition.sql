DROP TRIGGER blob_stores_configuration_in_use_update;

CREATE TRIGGER blob_stores_definition_immutable_update
BEFORE UPDATE OF driver, configuration_env, configuration_file, physical_identity ON blob_stores
WHEN OLD.driver <> NEW.driver
    OR OLD.configuration_env <> NEW.configuration_env
    OR OLD.configuration_file <> NEW.configuration_file
    OR OLD.physical_identity <> NEW.physical_identity
BEGIN
    SELECT RAISE(ABORT, 'blob store definition immutable');
END;
