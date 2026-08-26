ALTER TABLE adc_users
    ADD COLUMN authz_version BIGINT NOT NULL DEFAULT 1,
    ADD CONSTRAINT chk_users_authz_version_positive CHECK (authz_version > 0);

CREATE FUNCTION adc_bump_user_authz_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE adc_users SET authz_version = authz_version + 1 WHERE id = NEW.user_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE adc_users SET authz_version = authz_version + 1 WHERE id = OLD.user_id;
    ELSIF OLD.user_id = NEW.user_id THEN
        UPDATE adc_users SET authz_version = authz_version + 1 WHERE id = NEW.user_id;
    ELSE
        UPDATE adc_users SET authz_version = authz_version + 1 WHERE id IN (OLD.user_id, NEW.user_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_user_roles_bump_authz_version
AFTER INSERT OR UPDATE OR DELETE ON adc_user_roles
FOR EACH ROW EXECUTE FUNCTION adc_bump_user_authz_version();

CREATE FUNCTION adc_bump_authz_version_on_user_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.authz_version := OLD.authz_version + 1;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_users_state_bump_authz_version
BEFORE UPDATE OF status, deleted_at ON adc_users
FOR EACH ROW
WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at)
EXECUTE FUNCTION adc_bump_authz_version_on_user_state();
