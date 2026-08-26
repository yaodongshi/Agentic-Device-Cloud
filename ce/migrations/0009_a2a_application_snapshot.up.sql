-- Preserve A2A task history when a developer application is deleted.
-- application_id remains an immutable audit snapshot; a trigger enforces
-- tenant ownership on writes without blocking application lifecycle changes.

ALTER TABLE adc_a2a_tasks
    DROP CONSTRAINT IF EXISTS fk_a2a_tasks_application_tenant;

ALTER TABLE adc_developer_applications
    DROP CONSTRAINT IF EXISTS uq_developer_applications_id_tenant;

CREATE FUNCTION adc_validate_a2a_application_tenant() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM adc_developer_applications
        WHERE id = NEW.application_id AND tenant_id = NEW.tenant_id
    ) THEN
        RAISE EXCEPTION 'A2A application tenant mismatch' USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_a2a_application_tenant
BEFORE INSERT OR UPDATE OF application_id, tenant_id ON adc_a2a_tasks
FOR EACH ROW EXECUTE FUNCTION adc_validate_a2a_application_tenant();
