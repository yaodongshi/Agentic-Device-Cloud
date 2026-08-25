-- Development rollback only; production rollback keeps forward-compatible schema.
DROP TABLE IF EXISTS adc_developer_application_credentials;
DROP TABLE IF EXISTS adc_developer_applications;
