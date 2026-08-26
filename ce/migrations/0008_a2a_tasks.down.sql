DROP TABLE IF EXISTS adc_a2a_tasks;
ALTER TABLE adc_developer_applications
    DROP CONSTRAINT IF EXISTS uq_developer_applications_id_tenant;
