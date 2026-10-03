package webhooks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Payload is Meta's top-level webhook envelope. Only the fields the app acts on
// are decoded; Meta sends more, and unknown fields are ignored on purpose so a
// new Meta field cannot break an existing deployment.
type Payload struct {
	Object string  `json:"object"`
	Entry  []Entry `json:"entry"`
}

// Entry is one business account the change belongs to.
type Entry struct {
	ID      string   `json:"id"`
	Changes []Change `json:"changes"`
}

// Change is a single field update. Field is the routing key: "messages",
// "message_template_status_update", or "account_update". Value stays raw so a
// field the app does not understand is skipped without parsing it.
type Change struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

// Fields Meta sends on the WhatsApp webhook.
const (
	FieldMessages     = "messages"
	FieldTemplates    = "message_template_status_update"
	FieldAccountState = "account_update"
)

// MessageChange carries inbound messages and delivery status updates.
type MessageChange struct {
	// MessagingAccountID is the WABA the change belongs to.
	MessagingAccountID string `json:"messaging_account_id"`
	Metadata           struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		PhoneNumberID      string `json:"phone_number_id"`
	} `json:"metadata"`
	// Statuses are present for sent/delivered/read/failed updates.
	Statuses []MessageStatus `json:"statuses"`
	// Messages are present for inbound customer messages. The app records their
	// acknowledgement but does not yet auto-reply; that arrives with campaigns.
	Messages []InboundMessage `json:"messages"`
	Errors   []ChangeError    `json:"errors"`
}

// MessageStatus is one delivery status transition for a sent message.
type MessageStatus struct {
	ID          string        `json:"id"`
	RecipientID string        `json:"recipient_id"`
	Status      string        `json:"status"`
	Timestamp   string        `json:"timestamp"`
	Errors      []ChangeError `json:"errors"`
}

// InboundMessage is a customer message. Used only for its identity so far.
type InboundMessage struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	Timestamp string   `json:"timestamp"`
	Type      string   `json:"type"`
	Context   *Context `json:"context,omitempty"`
}

// Context identifies the message a reply refers to.
type Context struct {
	From string `json:"from"`
	ID   string `json:"id"`
}

// ChangeError is Meta's error object, present on failed statuses and on
// webhook-level errors.
type ChangeError struct {
	Code      int        `json:"code"`
	Title     string     `json:"title"`
	Message   string     `json:"message"`
	ErrorData *ErrorData `json:"error_data,omitempty"`
	Details   string     `json:"details,omitempty"`
	Href      string     `json:"href,omitempty"`
}

// ErrorData carries the recipient a failed status belongs to.
type ErrorData struct {
	Details string `json:"details"`
}

// AccountUpdate is sent when a business changes access to a phone number, for
// example when a customer removes the app's access.
type AccountUpdate struct {
	// Event is "PARTNER_ADDED", "PARTNER_REMOVED", or similar. TODO(META-DOC):
	// Meta does not document a closed set of values here, so unknown events are
	// ignored rather than treated as a removal.
	Event string `json:"event"`
	// PhoneNumberID identifies the number whose access changed.
	PhoneNumberID string `json:"phone_number_id"`
	// MessagingAccountID is the WABA, used to find the connection when the
	// change carries no phone number.
	MessagingAccountID string `json:"messaging_account_id"`
}

// TemplateStatusUpdate reports the review state of a submitted template.
type TemplateStatusUpdate struct {
	// Event is "APPROVED", "REJECTED", "PAUSED", or "PENDING".
	Event string `json:"event"`
	// MessageTemplateID is Meta's template id, stored as meta_template_id.
	MessageTemplateID   string `json:"message_template_id"`
	MessageTemplateName string `json:"message_template_name"`
	Reason              string `json:"reason"`
	Language            string `json:"language"`
}

// Meta's template review states, mapped onto templates.status.
const (
	TemplatePending  = "pending"
	TemplateApproved = "approved"
	TemplateRejected = "rejected"
	TemplatePaused   = "paused"
)

// TemplateStatus maps Meta's event name to our stored status. ok is false for
// anything we do not recognise, so a new Meta event is ignored rather than
// overwriting a known status with nonsense.
func TemplateStatus(event string) (status string, ok bool) {
	switch event {
	case "APPROVED":
		return TemplateApproved, true
	case "REJECTED", "DISAPPROVED":
		return TemplateRejected, true
	case "PAUSED":
		return TemplatePaused, true
	case "PENDING", "PENDING_TEMPLATE", "PENDING_REVIEW", "IN_APPEAL", "LIMITED":
		return TemplatePending, true
	default:
		return "", false
	}
}

// Message delivery states Meta reports, mapped onto message_logs.status.
const (
	StatusQueued    = "queued"
	StatusSent      = "sent"
	StatusDelivered = "delivered"
	StatusRead      = "read"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// MessageStatusRank is gone on purpose: the ordering rule lives in the SQL
// transition matrix for AdvanceMessageStatus, so there is exactly one place that
// decides whether a transition is allowed. Do not re-add a second copy here.

// ParseTime decodes Meta's webhook timestamps.
//
// Meta sends Unix epoch seconds as a JSON string (for example "1767225599"),
// not RFC 3339. The RFC 3339 form is accepted as a fallback so a future change
// in Meta's format, or a hand-written test, cannot turn every timestamp into
// "now". A value we cannot read at all is reported as an error rather than
// guessed: the caller decides whether receipt time is good enough, because
// Meta redelivers on a non-2xx and a single bad row must not loop forever.
func ParseTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("webhooks: empty timestamp")
	}

	if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return time.Unix(epoch, 0).UTC(), nil
	}

	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("webhooks: parse timestamp %q: %w", value, err)
	}
	return t.UTC(), nil
}
