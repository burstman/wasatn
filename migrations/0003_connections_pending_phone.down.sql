-- Rolling back 0003 restores the NOT NULL constraints, which cannot hold for a
-- connection that was onboarded without a phone number. Rather than silently
-- deleting a customer's connection, refuse and let the operator decide.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM connections WHERE phone_number_id IS NULL OR phone_number IS NULL) THEN
        RAISE EXCEPTION
            'cannot roll back 0003: % connection(s) have no phone number; reconnect or delete them first',
            (SELECT count(*) FROM connections WHERE phone_number_id IS NULL);
    END IF;
END
$$;

DROP INDEX IF EXISTS connections_phone_number_id_key;

ALTER TABLE connections
    DROP CONSTRAINT IF EXISTS connections_active_has_phone;

ALTER TABLE connections
    DROP CONSTRAINT IF EXISTS connections_phone_e164;

ALTER TABLE connections
    ADD CONSTRAINT connections_phone_e164
    CHECK (phone_number ~ '^\+[1-9][0-9]{7,14}$');

ALTER TABLE connections
    ALTER COLUMN phone_number SET NOT NULL,
    ALTER COLUMN phone_number_id SET NOT NULL;

CREATE UNIQUE INDEX connections_phone_number_id_key
    ON connections (phone_number_id);

-- The 'pending_phone' label stays in the connection_status enum on purpose:
-- PostgreSQL parses ALTER TYPE ... DROP VALUE but does not implement it. The
-- label is simply unused, and the up migration's ADD VALUE IF NOT EXISTS makes
-- a later re-apply a no-op.