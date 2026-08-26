DROP TRIGGER IF EXISTS trg_users_state_bump_authz_version ON adc_users;
DROP FUNCTION IF EXISTS adc_bump_authz_version_on_user_state();
DROP TRIGGER IF EXISTS trg_user_roles_bump_authz_version ON adc_user_roles;
DROP FUNCTION IF EXISTS adc_bump_user_authz_version();
ALTER TABLE adc_users DROP CONSTRAINT IF EXISTS chk_users_authz_version_positive;
ALTER TABLE adc_users DROP COLUMN IF EXISTS authz_version;
