-- Workspace branding rows carry an organization_id and no user_id, which the
-- original user_id primary key made impossible to insert. Identity moves to one
-- partial unique index per scope, and a check keeps every row in exactly one.
ALTER TABLE user_branding DROP CONSTRAINT user_branding_pkey;
ALTER TABLE user_branding ALTER COLUMN user_id DROP NOT NULL;

CREATE UNIQUE INDEX user_branding_user_key ON user_branding (user_id) WHERE organization_id IS NULL;
CREATE UNIQUE INDEX user_branding_org_key ON user_branding (organization_id) WHERE organization_id IS NOT NULL;

ALTER TABLE user_branding
    ADD CONSTRAINT user_branding_one_scope CHECK ((user_id IS NULL) <> (organization_id IS NULL));
