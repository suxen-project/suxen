CREATE OR REPLACE FUNCTION suxen_guard_blob_store_change()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.name = 'default' THEN
            RAISE EXCEPTION 'default blob store immutable';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.driver <> NEW.driver
        OR OLD.configuration_env <> NEW.configuration_env
        OR OLD.configuration_file <> NEW.configuration_file
        OR OLD.physical_identity <> NEW.physical_identity
    THEN
        RAISE EXCEPTION 'blob store definition immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER blob_stores_configuration_in_use_update ON blob_stores;

CREATE TRIGGER blob_stores_definition_immutable_update
BEFORE UPDATE OF driver, configuration_env, configuration_file, physical_identity ON blob_stores
FOR EACH ROW EXECUTE FUNCTION suxen_guard_blob_store_change();
