DROP TRIGGER IF EXISTS trg_a2a_application_tenant ON adc_a2a_tasks;
DROP FUNCTION IF EXISTS adc_validate_a2a_application_tenant();

ALTER TABLE adc_developer_applications
    ADD CONSTRAINT uq_developer_applications_id_tenant UNIQUE (id, tenant_id);

ALTER TABLE adc_a2a_tasks
    ADD CONSTRAINT fk_a2a_tasks_application_tenant
    FOREIGN KEY (application_id, tenant_id)
    REFERENCES adc_developer_applications (id, tenant_id);
