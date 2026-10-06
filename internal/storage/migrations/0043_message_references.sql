-- references_header keeps the References header so a reply can carry the
-- whole thread chain. "references" itself is an SQL keyword.
ALTER TABLE messages ADD COLUMN references_header TEXT NOT NULL DEFAULT '';
