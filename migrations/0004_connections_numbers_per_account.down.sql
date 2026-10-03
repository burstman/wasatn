-- Restores the single-row-per-account key from 0001, which cannot hold more
-- than one phone number per WhatsApp Business Account. Reverting loses rows:
-- the table goes back to one row per (user_id, messaging_account_id).
DROP INDEX IF EXISTS connections_pending_account_key;

ALTER TABLE connections
    ADD CONSTRAINT connections_user_messaging_account_key UNIQUE (user_id, messaging_account_id);