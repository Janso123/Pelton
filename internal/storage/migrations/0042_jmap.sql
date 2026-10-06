ALTER TABLE accounts ADD COLUMN protocol TEXT NOT NULL DEFAULT 'imap';
ALTER TABLE accounts ADD COLUMN jmap_session_url TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN jmap_mail_account_id TEXT NOT NULL DEFAULT '';
