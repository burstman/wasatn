-- A WhatsApp Business Account can own several phone numbers, so a customer can
-- legitimately connect more than one number per account. Keying every row on
-- (user_id, messaging_account_id) meant a second number overwrote the first
-- during signup, silently leaving the customer with one number and no
-- indication that anything was lost.
--
-- Identity now follows the phone number, which is what Meta keys on:
--   * one row per (user, phone_number_id), which the table already enforced;
--   * at most one pending row per (user, messaging_account_id), so an account
--     waiting for its first number cannot accumulate a new row per signup.
--
-- Pending rows have a NULL phone_number_id, and Postgres treats NULLs as
-- distinct, so the phone-based unique constraint already permits them.

ALTER TABLE connections
    DROP CONSTRAINT IF EXISTS connections_user_messaging_account_key;

CREATE UNIQUE INDEX connections_pending_account_key
    ON connections (user_id, messaging_account_id)
    WHERE phone_number_id IS NULL;