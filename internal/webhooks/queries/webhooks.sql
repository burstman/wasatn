-- name: AdvanceMessageStatus :one
-- Records a delivery status transition on the message row identified by wamid.
--
-- Meta redelivers webhooks, and delivery statuses can arrive out of order, so
-- a transition must never move a message backwards.
--
-- The guard is computed once, in the subquery, and gates the status *and* every
-- timestamp. A transition that is not applied therefore has no effect at all,
-- which is what keeps failed_at honest for analytics.
--
-- A message we have never seen returns no row: :one reports pgx.ErrNoRows and
-- the handler answers 200, because a non-2xx would make Meta redeliver forever.
UPDATE message_logs AS m
SET status = CASE
        WHEN current.advances THEN @status
        ELSE m.status
    END,
    sent_at = CASE
        WHEN current.advances AND @status::message_status = 'sent' THEN COALESCE(m.sent_at, @at)
        ELSE m.sent_at
    END,
    delivered_at = CASE
        WHEN current.advances AND @status::message_status = 'delivered' THEN COALESCE(m.delivered_at, @at)
        ELSE m.delivered_at
    END,
    read_at = CASE
        WHEN current.advances AND @status::message_status = 'read' THEN COALESCE(m.read_at, @at)
        ELSE m.read_at
    END,
    failed_at = CASE
        WHEN current.advances AND @status::message_status = 'failed' THEN COALESCE(m.failed_at, @at)
        ELSE m.failed_at
    END,
    -- Keep the first error: a redelivered failure carries the same code.
    error_code = CASE
        WHEN current.advances AND @status::message_status = 'failed' THEN COALESCE(@error_code, m.error_code)
        ELSE m.error_code
    END,
    error_message = CASE
        WHEN current.advances AND @status::message_status = 'failed' THEN COALESCE(@error_message, m.error_message)
        ELSE m.error_message
    END
FROM (
    -- The transition matrix, in one place. Meta redelivers webhooks and
    -- delivery statuses can arrive out of order, so only forward moves are
    -- allowed: a redelivered "delivered" after "read" is ignored, and "failed"
    -- is terminal because it wins over every delivery state.
    SELECT id,
           CASE
               WHEN @status::message_status = status THEN false
               WHEN status = 'queued'
                   AND @status::message_status IN ('sent', 'delivered', 'read', 'failed') THEN true
               WHEN status = 'sent'
                   AND @status::message_status IN ('delivered', 'read', 'failed') THEN true
               WHEN status = 'delivered'
                   AND @status::message_status IN ('read', 'failed') THEN true
               WHEN status = 'read'
                   AND @status::message_status = 'failed' THEN true
               ELSE false
           END AS advances
    FROM message_logs
    WHERE message_logs.wamid = @wamid
) AS current
WHERE m.id = current.id
RETURNING m.*;

-- name: DisconnectConnectionAndPauseCampaigns :execrows
-- Marks a connection disconnected and pauses everything it was sending.
--
-- account_update arrives when a business removes the app's access. The payload
-- carries a phone number id, a WABA id, or both, so an empty argument means
-- "do not match on this column". Re-delivery is a no-op: a connection that is
-- already disconnected returns no rows, so its campaigns are not touched again.
WITH target AS (
    UPDATE connections
    SET status = 'disconnected',
        updated_at = now()
    WHERE (@phone_number_id::text = '' OR phone_number_id = @phone_number_id::text)
      AND (@messaging_account_id::text = '' OR messaging_account_id = @messaging_account_id::text)
      AND status <> 'disconnected'
    RETURNING id
)
UPDATE campaigns
SET status = 'paused',
    updated_at = now()
WHERE connection_id IN (SELECT id FROM target)
  AND status IN ('draft', 'scheduled', 'sending');

-- name: UpdateTemplateReviewStatus :execrows
-- Applies a template review status from Meta. The reason doubles as the
-- rejection reason shown in the UI.
UPDATE templates
SET status = @status::template_status,
    rejection_reason = @rejection_reason
WHERE meta_template_id = @meta_template_id;

-- name: GetConnectionByPhoneNumberID :one
SELECT *
FROM connections
WHERE phone_number_id = @phone_number_id;

-- name: GetMessageLogByWamid :one
SELECT *
FROM message_logs
WHERE wamid = @wamid;