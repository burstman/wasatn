package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/phone"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// nowFunc is swapped in tests so recorded timestamps are deterministic.
var nowFunc = func() time.Time { return time.Now().UTC() }

// ErrMalformedPayload marks a delivery we cannot decode. It answers 400 rather
// than 500 because Meta retries a 5xx: retrying a payload that can never be
// understood would loop until Meta gives up.
var ErrMalformedPayload = errors.New("webhooks: malformed payload")

// MaxBodyBytes caps the payload we will read. Meta's messages webhook is a few
// kilobytes; anything vastly larger is either a bug or an attempt to exhaust
// memory, and refusing early keeps the public endpoint cheap.
const MaxBodyBytes = 1 << 20 // 1 MiB

// Querier is the database surface the webhook handlers need. It is an interface
// so the handlers can be unit tested without Postgres, while production passes
// the generated sqlc.Queries.
type Querier interface {
	AdvanceMessageStatus(ctx context.Context, arg sqlc.AdvanceMessageStatusParams) (sqlc.MessageLog, error)
	DisconnectConnectionAndPauseCampaigns(ctx context.Context, arg sqlc.DisconnectConnectionAndPauseCampaignsParams) (int64, error)
	UpdateTemplateReviewStatus(ctx context.Context, arg sqlc.UpdateTemplateReviewStatusParams) (int64, error)
}

// Handler serves Meta's webhook endpoint.
//
// It is deliberately unauthenticated: Meta cannot present a session. Every
// delivery is authenticated by the HMAC signature instead, and the subscription
// challenge by the shared verify token.
type Handler struct {
	queries     Querier
	appSecret   string
	verifyToken string
	log         *slog.Logger
}

// New builds a Handler.
func New(queries Querier, appSecret, verifyToken string, log *slog.Logger) *Handler {
	return &Handler{
		queries:     queries,
		appSecret:   appSecret,
		verifyToken: verifyToken,
		log:         log.With("component", "webhooks"),
	}
}

// Routes registers the webhook endpoints on a chi-style router.
func (h *Handler) Routes(r interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}) {
	// The path Meta is configured with; see the README's Meta setup notes.
	r.Get("/webhooks/whatsapp", h.Challenge)
	r.Post("/webhooks/whatsapp", h.Receive)
}

// Challenge answers Meta's subscription verification request. Meta calls this
// once when the callback URL is saved, with the verify token we configured.
func (h *Handler) Challenge(w http.ResponseWriter, r *http.Request) {
	params := VerifyChallengeParams{
		Mode:      r.URL.Query().Get("hub.mode"),
		Token:     r.URL.Query().Get("hub.verify_token"),
		Challenge: r.URL.Query().Get("hub.challenge"),
	}
	if err := params.Validate(h.verifyToken); err != nil {
		// 403 is what Meta expects for a bad token, and the detail stays in our
		// logs rather than in the response.
		h.log.WarnContext(r.Context(), "rejected webhook challenge", "reason", err.Error())
		http.Error(w, "verification failed", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Echoed verbatim; Meta compares it byte for byte.
	fmt.Fprint(w, params.Challenge)
}

// Receive handles a signed delivery.
func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		// A truncated or oversized body cannot be verified, so there is nothing
		// to process. 400 tells Meta the request was malformed.
		h.log.WarnContext(r.Context(), "cannot read webhook body", "err", err.Error())
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	// Verify before parsing: an unauthenticated body must never reach a query.
	if err := VerifySignature(h.appSecret, body, r.Header.Get(SignatureHeader)); err != nil {
		h.log.WarnContext(r.Context(), "rejected webhook delivery", "reason", err.Error())
		if errors.Is(err, ErrSignatureMissing) || errors.Is(err, ErrSignatureMalformed) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}

	var payload Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.log.WarnContext(r.Context(), "malformed webhook payload", "err", err.Error())
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	if err := h.process(r.Context(), payload); err != nil {
		h.log.ErrorContext(r.Context(), "webhook processing failed", "err", err.Error())
		if errors.Is(err, ErrMalformedPayload) {
			http.Error(w, "malformed payload", http.StatusBadRequest)
			return
		}
		// A database failure must surface as 500 so Meta redelivers the delivery
		// we could not apply. Swallowing it would lose a status for good.
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}

	// 200 for everything we understood, including deliveries about messages we
	// have never seen: a 404 here would make Meta redeliver the same payload
	// for days.
	w.WriteHeader(http.StatusOK)
}

// process dispatches every change in the payload.
func (h *Handler) process(ctx context.Context, payload Payload) error {
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if err := h.dispatch(ctx, change); err != nil {
				return fmt.Errorf("field %q: %w", change.Field, err)
			}
		}
	}
	return nil
}

// dispatch routes one change by its field. Unknown fields are ignored rather
// than rejected: Meta adds fields regularly, and an unknown one must not fail
// the whole delivery.
func (h *Handler) dispatch(ctx context.Context, change Change) error {
	switch change.Field {
	case FieldMessages:
		var value MessageChange
		if err := decode(change.Value, &value); err != nil {
			return err
		}
		return h.handleMessages(ctx, value)
	case FieldAccountState:
		var value AccountUpdate
		if err := decode(change.Value, &value); err != nil {
			return err
		}
		return h.handleAccountUpdate(ctx, value)
	case FieldTemplates:
		var value TemplateStatusUpdate
		if err := decode(change.Value, &value); err != nil {
			return err
		}
		return h.handleTemplateStatus(ctx, value)
	default:
		h.log.DebugContext(ctx, "ignoring unknown webhook field", "field", change.Field)
		return nil
	}
}

// decode unmarshals a change's raw value, marking the failure as a malformed
// payload so the caller answers 400 instead of asking Meta to retry forever.
func decode(raw json.RawMessage, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	return nil
}

// handleMessages applies delivery status updates. Inbound customer messages are
// only counted for now; replying arrives with the campaign engine.
func (h *Handler) handleMessages(ctx context.Context, value MessageChange) error {
	for _, status := range value.Statuses {
		if status.ID == "" {
			// Without a wamid there is no row to update.
			h.log.DebugContext(ctx, "skipping status without an id")
			continue
		}

		stored, ok := normaliseStatus(status.Status)
		if !ok {
			h.log.WarnContext(ctx, "ignoring unknown message status",
				"status", status.Status, "wamid_suffix", wamidSuffix(status.ID))
			continue
		}

		at, err := ParseTime(status.Timestamp)
		if err != nil {
			// Fall back to now rather than dropping the transition: an
			// unparseable timestamp still tells us the message reached this
			// stage, and a wrong second is better than a lost delivery.
			h.log.WarnContext(ctx, "bad status timestamp, using receipt time",
				"err", err.Error(), "wamid_suffix", wamidSuffix(status.ID))
			at = nowFunc()
		}

		code, message := firstError(status.Errors)
		params := sqlc.AdvanceMessageStatusParams{
			Wamid:        &status.ID,
			Status:       sqlc.MessageStatus(stored),
			At:           pgtype.Timestamptz{Time: at, Valid: true},
			ErrorCode:    code,
			ErrorMessage: message,
		}

		_, err = h.queries.AdvanceMessageStatus(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			// We did not send this message, so there is nothing to update. This
			// is normal for statuses on a number we no longer track.
			h.log.DebugContext(ctx, "status for unknown message",
				"wamid_suffix", wamidSuffix(status.ID))
			continue
		}
		if err != nil {
			return fmt.Errorf("advance %s to %s: %w", wamidSuffix(status.ID), stored, err)
		}
	}

	for _, msg := range value.Messages {
		// Never log the sender in full: PROMPT.md requires masking.
		h.log.DebugContext(ctx, "received inbound message",
			"wamid_suffix", wamidSuffix(msg.ID),
			"from", phone.Mask(msg.From),
			"type", msg.Type,
		)
	}

	return nil
}

// handleAccountUpdate reacts to a business changing the app's access. Losing
// access means we can no longer send, so the connection is marked disconnected
// and its campaigns are paused rather than left failing one recipient at a time.
func (h *Handler) handleAccountUpdate(ctx context.Context, value AccountUpdate) error {
	if value.PhoneNumberID == "" && value.MessagingAccountID == "" {
		// Nothing to match on. Ignoring is safer than guessing.
		h.log.WarnContext(ctx, "account_update without an identifier", "event", value.Event)
		return nil
	}

	// TODO(META-DOC): Meta does not document a closed set of event values, so
	// only a removal is acted on. Unknown events are logged and ignored rather
	// than disconnecting a paying customer on a guess.
	if !isRemovalEvent(value.Event) {
		h.log.DebugContext(ctx, "ignoring non-removal account_update", "event", value.Event)
		return nil
	}

	params := sqlc.DisconnectConnectionAndPauseCampaignsParams{
		PhoneNumberID:      value.PhoneNumberID,
		MessagingAccountID: value.MessagingAccountID,
	}
	rows, err := h.queries.DisconnectConnectionAndPauseCampaigns(ctx, params)
	if err != nil {
		return fmt.Errorf("disconnect connection: %w", err)
	}
	if rows == 0 {
		// Already disconnected, or a number we never had.
		h.log.DebugContext(ctx, "account_update matched no active connection",
			"event", value.Event)
	}
	return nil
}

// handleTemplateStatus records Meta's review decision on a template.
func (h *Handler) handleTemplateStatus(ctx context.Context, value TemplateStatusUpdate) error {
	if value.MessageTemplateID == "" {
		h.log.DebugContext(ctx, "skipping template status without an id")
		return nil
	}

	status, ok := TemplateStatus(value.Event)
	if !ok {
		h.log.DebugContext(ctx, "ignoring unknown template event", "event", value.Event)
		return nil
	}

	// A rejection with no reason would show an empty badge in the UI, so keep
	// whatever Meta sent and let the page decide how to present it.
	var reason *string
	if trimmed := strings.TrimSpace(value.Reason); trimmed != "" {
		reason = &trimmed
	}

	params := sqlc.UpdateTemplateReviewStatusParams{
		MetaTemplateID:  &value.MessageTemplateID,
		Status:          sqlc.TemplateStatus(status),
		RejectionReason: reason,
	}
	if _, err := h.queries.UpdateTemplateReviewStatus(ctx, params); err != nil {
		return fmt.Errorf("update template %s: %w", value.MessageTemplateID, err)
	}
	return nil
}

// normaliseStatus maps Meta's status string onto a stored status. Meta uses the
// same words we store, but mapping them explicitly means a new Meta value is
// ignored instead of being written as an invalid enum and failing the delivery.
func normaliseStatus(status string) (string, bool) {
	switch status {
	case StatusSent, StatusDelivered, StatusRead, StatusFailed:
		return status, true
	default:
		return "", false
	}
}

// isRemovalEvent reports whether an account_update event means the app lost
// access to the number.
func isRemovalEvent(event string) bool {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "PARTNER_REMOVED", "REMOVED", "ACCESS_REVOKED":
		return true
	default:
		return false
	}
}

// firstError returns the first error Meta attached to a status, if any. Either
// result may be nil, which the query turns into "keep what we had".
func firstError(errs []ChangeError) (code, message *string) {
	if len(errs) == 0 {
		return nil, nil
	}

	first := errs[0]
	if first.Code != 0 {
		text := strconv.Itoa(first.Code)
		code = &text
	}

	// Prefer the specific detail over Meta's generic title.
	text := first.Message
	if text == "" {
		text = first.Title
	}
	if first.ErrorData != nil && first.ErrorData.Details != "" {
		text = first.ErrorData.Details
	}
	if text != "" {
		message = &text
	}
	return code, message
}

// wamidSuffix returns the last few characters of a wamid, enough to correlate
// logs without writing a full message id into them.
func wamidSuffix(id string) string {
	const keep = 8
	if len(id) <= keep {
		return id
	}
	return id[len(id)-keep:]
}
