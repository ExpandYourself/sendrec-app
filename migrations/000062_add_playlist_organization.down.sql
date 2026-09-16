-- Workspace playlists cannot be represented without the column; they return to
-- the member who created them.
DROP INDEX IF EXISTS idx_playlists_organization_id;
ALTER TABLE playlists DROP COLUMN organization_id;
