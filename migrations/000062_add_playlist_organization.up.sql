-- Playlists predate workspaces and were the only content type still keyed to a
-- single user. Existing rows stay personal (NULL), matching how they behave now.
ALTER TABLE playlists ADD COLUMN organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
CREATE INDEX idx_playlists_organization_id ON playlists(organization_id);
