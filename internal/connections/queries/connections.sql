-- name: ListConnectionsByUser :many
-- Every connection the signed-in user owns, newest first.
SELECT *
FROM connections
WHERE user_id = @user_id
ORDER BY created_at DESC;

-- name: CountConnectionsByUser :one
-- Dashboard counter. Every status counts: a disconnected number is still a
-- number the user connected, and hiding it would make the total fall when a
-- customer is trying to work out what went wrong.
SELECT count(*)
FROM connections
WHERE user_id = @user_id;

-- name: GetConnectionByIDForUser :one
-- Scoped by user_id on purpose: a connection id from the URL is untrusted, so
-- the ownership check happens in the query rather than in the handler.
SELECT *
FROM connections
WHERE id = @id
  AND user_id = @user_id;

-- name: GetPendingConnectionByUserAndWABA :one
-- Used when a signup returns an account with no phone numbers, to tell a first
-- onboarding apart from a reconnect that lost a number.
--
-- Scoped to rows with no phone number: once an account has numbers, it has one
-- row per number and none of them are pending.
SELECT *
FROM connections
WHERE user_id = @user_id
  AND messaging_account_id = @messaging_account_id
  AND phone_number_id IS NULL;

-- name: UpsertConnection :one
-- Stores one phone number as a connection.
--
-- A WhatsApp Business Account can own several numbers, so the conflict target is
-- the phone number rather than the account: reconnecting updates the row for
-- that number, and a second number on the same account becomes a second row
-- instead of overwriting the first.
--
-- A conflict on the global phone_number_id index is deliberately not handled
-- here: that index exists so one WhatsApp number can never belong to two
-- tenants, and a violation is a real error the handler turns into a 409.
INSERT INTO connections (
    user_id,
    messaging_account_id,
    waac_id,
    phone_number_id,
    phone_number,
    display_name,
    access_token_encrypted,
    token_expires_at,
    quality_rating,
    messaging_limit,
    status
)
VALUES (
    @user_id,
    @messaging_account_id,
    @waac_id,
    @phone_number_id,
    @phone_number,
    @display_name,
    @access_token_encrypted,
    @token_expires_at,
    @quality_rating,
    @messaging_limit,
    @status::connection_status
)
ON CONFLICT (user_id, phone_number_id) DO UPDATE
SET waac_id               = EXCLUDED.waac_id,
    phone_number_id       = EXCLUDED.phone_number_id,
    phone_number          = EXCLUDED.phone_number,
    display_name          = EXCLUDED.display_name,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    token_expires_at      = EXCLUDED.token_expires_at,
    quality_rating        = EXCLUDED.quality_rating,
    messaging_limit       = EXCLUDED.messaging_limit,
    status                = EXCLUDED.status,
    updated_at            = now()
RETURNING *;

-- name: UpsertPendingConnection :one
-- Stores an account that has no phone number yet, as Embedded Signup v4 allows.
--
-- Keyed on the account because there is no number to key on. The partial unique
-- index behind it is what stops a customer pressing the signup button twice
-- from collecting a duplicate pending row.
--
-- Reconnecting brings a new token and expiry, so those columns are updated too.
INSERT INTO connections (
    user_id,
    messaging_account_id,
    waac_id,
    access_token_encrypted,
    token_expires_at,
    quality_rating,
    status
)
VALUES (
    @user_id,
    @messaging_account_id,
    @waac_id,
    @access_token_encrypted,
    @token_expires_at,
    'UNKNOWN',
    @status::connection_status
)
ON CONFLICT (user_id, messaging_account_id) WHERE phone_number_id IS NULL DO UPDATE
SET waac_id                = EXCLUDED.waac_id,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    token_expires_at       = EXCLUDED.token_expires_at,
    quality_rating         = EXCLUDED.quality_rating,
    status                 = EXCLUDED.status,
    updated_at             = now()
RETURNING *;

-- name: DeletePendingConnectionForAccount :execrows
-- Drops the placeholder row for an account that has just gained a number.
--
-- Without this the customer would see both "Needs a number" and the number they
-- just connected for the same business account, and the placeholder would keep
-- claiming a token that nothing uses.
DELETE FROM connections
WHERE user_id = @user_id
  AND messaging_account_id = @messaging_account_id
  AND phone_number_id IS NULL;

-- name: DisconnectOwnedConnectionAndPauseCampaigns :one
-- Marks a connection disconnected and pauses everything it was sending.
--
-- The same thing happens when Meta reports the customer removed access, so the
-- rule lives in one place: a disconnected connection must never keep a campaign
-- in a state that would try to send from it. Pausing only draft, scheduled and
-- sending campaigns leaves finished history alone.
--
-- `changed` is false when the connection was already disconnected, which is how
-- the handler distinguishes a no-op retry from a real disconnect.
WITH target AS (
    UPDATE connections
    SET status     = 'disconnected',
        updated_at = now()
    WHERE connections.id = @id
      AND connections.user_id = @user_id
      AND connections.status <> 'disconnected'
    RETURNING connections.id
),
paused AS (
    UPDATE campaigns
    SET status     = 'paused',
        updated_at = now()
    WHERE connection_id IN (SELECT id FROM target)
      AND status IN ('draft', 'scheduled', 'sending')
    RETURNING id
)
SELECT EXISTS (SELECT 1 FROM target) AS changed,
       (SELECT count(*) FROM paused)::bigint AS paused_campaigns;