-- Embedded Signup v4 lets a business customer finish the flow with a verified,
-- an unverified, or no phone number at all (v2 guaranteed a verified number).
-- A connection is therefore created as soon as the WABA is known, and gains its
-- phone number later, so phone_number_id and phone_number become nullable and
-- 'pending_phone' records an onboarding that is not sendable yet.

ALTER TYPE connection_status ADD VALUE IF NOT EXISTS 'pending_phone';

ALTER TABLE connections
    ALTER COLUMN phone_number_id DROP NOT NULL,
    ALTER COLUMN phone_number DROP NOT NULL;

-- The E.164 check cannot run on NULL, so guard it: an absent number is legal,
-- a present one must still be E.164.
ALTER TABLE connections
    DROP CONSTRAINT IF EXISTS connections_phone_e164;

ALTER TABLE connections
    ADD CONSTRAINT connections_phone_e164
    CHECK (phone_number IS NULL OR phone_number ~ '^\+[1-9][0-9]{7,14}$');

-- One phone number per connection is the identity Meta keys on, so it stays
-- unique once present. NULLs are excluded, matching the other partial indexes.
CREATE UNIQUE INDEX connections_phone_number_id_key
    ON connections (phone_number_id)
    WHERE phone_number_id IS NOT NULL;

-- Only an active connection can send, so only an active connection needs a
-- phone number. A disconnected or token-expired row may have lost it.
ALTER TABLE connections
    ADD CONSTRAINT connections_active_has_phone
    CHECK (status <> 'active' OR phone_number_id IS NOT NULL);