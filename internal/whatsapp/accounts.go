package whatsapp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// BusinessAccount is a WhatsApp Business Account (WABA) the customer's token
// can manage.
type BusinessAccount struct {
	// ID is the messaging_account_id stored on the connection.
	ID string
	// Name is the account's display name in the Business Manager.
	Name string
	// AccessToken is a token already scoped to this WABA.
	//
	// /me/accounts returns one per account rather than one for the user, and
	// using it for this account's calls is what keeps one customer's WABA
	// updates from touching another's.
	AccessToken string
}

// accountsResponse is the paged /me/accounts payload.
type accountsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		AccessToken string `json:"access_token"`
	} `json:"data"`
	Paging struct {
		Next string `json:"next"`
	} `json:"paging"`
}

// BusinessAccountFields are the /me/accounts fields Embedded Signup needs.
const BusinessAccountFields = "id,name,access_token"

// BusinessAccounts lists every WhatsApp Business Account the token can reach.
//
// A single signup can grant access to several accounts, which is why this
// returns a list rather than the first match: picking one silently would drop
// numbers the customer connected on purpose.
func (c *Client) BusinessAccounts(ctx context.Context, token string) ([]BusinessAccount, error) {
	if err := requireToken(token); err != nil {
		return nil, err
	}

	// Paging is followed by hand. The list is small in practice, but a truncated
	// list is worse than a few extra requests: the customer would believe a
	// connected number was saved when it was not.
	const maxPages = 20
	// baseQuery stays constant: a cursor from a previous page must not be
	// carried into the next one, or the merge below would keep the old value.
	baseQuery := url.Values{
		"fields":       {BusinessAccountFields},
		"access_token": {token},
	}
	var (
		accounts []BusinessAccount
		path     = "me/accounts"
		query    = baseQuery
	)

	for page := 0; path != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("whatsapp: business account list did not terminate after %d pages", maxPages)
		}

		var resp accountsResponse
		if err := c.get(ctx, path, query, &resp); err != nil {
			return nil, fmt.Errorf("whatsapp: list business accounts: %w", err)
		}

		for _, raw := range resp.Data {
			if strings.TrimSpace(raw.ID) == "" {
				continue
			}
			account := BusinessAccount{
				ID:          raw.ID,
				Name:        raw.Name,
				AccessToken: strings.TrimSpace(raw.AccessToken),
			}
			// Fall back to the user token: Graph omits the per-account token
			// when it is identical, and an empty one would break every call.
			if account.AccessToken == "" {
				account.AccessToken = token
			}
			accounts = append(accounts, account)
		}

		path, query = nextPage(resp.Paging.Next, baseQuery)
	}

	return accounts, nil
}

// PhoneNumber is a number registered on a WhatsApp Business Account.
type PhoneNumber struct {
	// ID is the phone_number_id used to send messages.
	ID string
	// DisplayPhoneNumber is what the customer recognises, e.g. "+1 555 010 9999".
	DisplayPhoneNumber string
	// VerifiedName is the name approved for the number, shown as the sender.
	VerifiedName string
	// QualityRating is Meta's GREEN/YELLOW/RED/UNKNOWN signal.
	QualityRating string
	// Status is the registration state, e.g. "CONNECTED" or "PENDING".
	Status string
	// IsOfficialBusinessAccount reports whether the number is the business's own
	// rather than a display name on a shared number. WasaTN only offers sending
	// on official business accounts, so this gates the connection.
	IsOfficialBusinessAccount bool
	// NameStatus is "APPROVED" or "PENDING_VERIFICATION".
	NameStatus string
	// AccountMode is "LIVE" or "SANDBOX".
	AccountMode string
	// MessagingLimit is the current tier limit, recorded for display.
	MessagingLimit int
}

// PhoneNumberFields are the phone_numbers fields Embedded Signup needs.
const PhoneNumberFields = "id,display_phone_number,verified_name,quality_rating," +
	"status,is_official_business_account,name_status,account_mode,messaging_limit"

// phoneNumbersResponse is the /{waba}/phone_numbers payload. Only the fields
// WasaTN stores are decoded; the rest of Meta's response is ignored on purpose,
// since unused fields would be dead weight to keep in sync.
type phoneNumbersResponse struct {
	Data []struct {
		ID                        string `json:"id"`
		DisplayPhoneNumber        string `json:"display_phone_number"`
		VerifiedName              string `json:"verified_name"`
		QualityRating             string `json:"quality_rating"`
		Status                    string `json:"status"`
		IsOfficialBusinessAccount bool   `json:"is_official_business_account"`
		NameStatus                string `json:"name_status"`
		NewNameStatus             string `json:"new_name_status"`
		AccountMode               string `json:"account_mode"`
		CurrentMessagingLimit     int    `json:"current_messaging_limit"`
	} `json:"data"`
	Paging struct {
		Next string `json:"next"`
	} `json:"paging"`
}

// PhoneNumbers lists the numbers registered on a WhatsApp Business Account.
//
// An account with no numbers yet is not an error: Embedded Signup v4 lets a
// business finish onboarding before a number is verified, so the caller records
// a pending_phone connection instead.
func (c *Client) PhoneNumbers(ctx context.Context, wabaID, token string) ([]PhoneNumber, error) {
	if strings.TrimSpace(wabaID) == "" {
		return nil, fmt.Errorf("whatsapp: business account id is required")
	}
	if err := requireToken(token); err != nil {
		return nil, err
	}

	const maxPages = 20
	baseQuery := url.Values{
		"fields":       {PhoneNumberFields},
		"access_token": {token},
	}
	var (
		numbers []PhoneNumber
		path    = "/" + wabaID + "/phone_numbers"
		query   = baseQuery
	)

	for page := 0; path != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("whatsapp: phone number list did not terminate after %d pages", maxPages)
		}

		var resp phoneNumbersResponse
		if err := c.get(ctx, path, query, &resp); err != nil {
			return nil, fmt.Errorf("whatsapp: list phone numbers: %w", err)
		}

		for _, raw := range resp.Data {
			if strings.TrimSpace(raw.ID) == "" {
				continue
			}
			numbers = append(numbers, PhoneNumber{
				ID:                        raw.ID,
				DisplayPhoneNumber:        raw.DisplayPhoneNumber,
				VerifiedName:              raw.VerifiedName,
				QualityRating:             qualityRatingOrDefault(raw.QualityRating),
				Status:                    raw.Status,
				IsOfficialBusinessAccount: raw.IsOfficialBusinessAccount,
				NameStatus:                firstNonEmpty(raw.NameStatus, raw.NewNameStatus),
				AccountMode:               raw.AccountMode,
				MessagingLimit:            raw.CurrentMessagingLimit,
			})
		}

		path, query = nextPage(resp.Paging.Next, baseQuery)
	}

	return numbers, nil
}

// qualityRatingOrDefault keeps the column's default meaningful when Meta omits
// the field, which it does for numbers that have never sent.
func qualityRatingOrDefault(rating string) string {
	if rating = strings.TrimSpace(rating); rating != "" {
		return rating
	}
	return "UNKNOWN"
}

// SubscribeApp subscribes the WasaTN app to a WABA's webhooks.
//
// Without this, message and template status updates never arrive, so a
// connection would send but never track delivery. It is idempotent per Meta.
func (c *Client) SubscribeApp(ctx context.Context, wabaID, token string) error {
	if strings.TrimSpace(wabaID) == "" {
		return fmt.Errorf("whatsapp: business account id is required")
	}
	if err := requireToken(token); err != nil {
		return err
	}

	form := url.Values{"access_token": {token}}
	if err := c.post(ctx, "/"+wabaID+"/subscribed_apps", form, nil); err != nil {
		return fmt.Errorf("whatsapp: subscribe app to business account: %w", err)
	}
	return nil
}

// requireToken rejects an empty token before it becomes a request that Graph
// answers with an opaque error.
func requireToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("whatsapp: access token is required")
	}
	return nil
}

// nextPage converts a paging.next URL into the path and query the following
// request should use.
//
// The cursor (after/before) lives in that URL's query string, so the query has
// to be carried over as well as the path: dropping it would fetch page one
// forever. baseParams are the request's own parameters and win over anything in
// the URL, so a paging link cannot substitute a different access token.
func nextPage(next string, baseParams url.Values) (string, url.Values) {
	query := url.Values{}
	for key, values := range baseParams {
		query[key] = append([]string(nil), values...)
	}

	next = strings.TrimSpace(next)
	if next == "" {
		return "", query
	}

	parsed, err := url.Parse(next)
	if err != nil {
		// Unparseable: hand it to the request and let the failure surface there
		// rather than looping on page one.
		return next, query
	}
	for key, values := range parsed.Query() {
		if _, set := query[key]; !set {
			query[key] = values
		}
	}

	path := strings.TrimPrefix(parsed.Path, "/")
	// Strip the version segment, which endpoint adds back.
	if parts := strings.SplitN(path, "/", 2); len(parts) == 2 && isVersionSegment(parts[0]) {
		path = parts[1]
	}
	return path, query
}

// isVersionSegment reports whether s is a Graph version such as "v25.0".
func isVersionSegment(s string) bool {
	if !strings.HasPrefix(s, "v") {
		return false
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(s, "v"), ".")
	return isDigits(major)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
