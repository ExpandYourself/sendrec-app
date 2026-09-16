-- Workspace branding cannot be represented once user_id is mandatory again.
DELETE FROM user_branding WHERE user_id IS NULL;

ALTER TABLE user_branding DROP CONSTRAINT user_branding_one_scope;
DROP INDEX user_branding_org_key;
DROP INDEX user_branding_user_key;

ALTER TABLE user_branding ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE user_branding ADD CONSTRAINT user_branding_pkey PRIMARY KEY (user_id);
