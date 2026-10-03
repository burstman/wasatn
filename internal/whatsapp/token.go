package whatsapp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ShortLivedTokenLifetime is how long the token from the first exchange lasts.
//
// Meta does not state it in the response, only in the documentation, and it is
// never stored: it exists solely to be traded for the long-lived token. The
// constant documents the assumption rather than enforcing it.
const ShortLivedTokenLifetime = 24 * time.Hour

// Token is a long-lived WhatsApp Business access token.
//
// It is customer-scoped: it grants access to one business's WhatsApp Account and
// nothing else, which is what makes WasaTN multi-tenant. It cannot be upgraded
// into a Business Manager system user token, so it expires and the customer
// reconnects (see Service.ReconnectExpired).
type Token struct {
	AccessToken string
	// ExpiresAt is when Meta stops accepting the token. connections.token_expires_at
	// is NOT NULL, so an unknown expiry cannot be stored.
	ExpiresAt time.Time
}

// Valid reports whether the token is still usable at now, with a small margin.
//
// The margin matters: a token that dies mid-send produces a campaign whose
// messages fail with an opaque error, so the check is made early enough to warn
// while the customer can still act on it.
func (t Token) Valid(now time.Time) bool {
	if t.AccessToken == "" {
		return false
	}
	return t.ExpiresAt.After(now.Add(TokenExpiryWarningWindow))
}

// TokenExpiryWarningWindow is how far ahead a token counts as expiring. It
// matches the 60 day lifetime Meta grants to a business token closely enough to
// give a customer time to reconnect without the warning being noise.
const TokenExpiryWarningWindow = 7 * 24 * time.Hour

// tokenResponse is the body of /oauth/access_token.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	// ExpiresIn is seconds and is present on the long-lived exchange. It is
	// absent on the short-lived one, so it must not be required there.
	ExpiresIn int64 `json:"expires_in"`
}

// ExchangeCode trades a Facebook Login authorization code for a long-lived
// access token.
//
// Classic Embedded Signup hands the browser a single-use code. It is exchanged
// twice, because Meta's own flow requires it:
//
//  1. the code is swapped for a short-lived (~24h) token;
//  2. that token is swapped again with grant_type=fb_exchange_token for the
//     ~60 day token that is actually stored.
//
// Only the second response carries expires_in. Doing the second exchange is what
// makes the connection last more than a day, and skipping it would silently
// produce a connection that dies overnight.
//
// redirectURI must be byte-identical to the URI the code was requested with,
// because Meta rejects a mismatch.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (Token, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Token{}, fmt.Errorf("whatsapp: authorization code is required")
	}
	if strings.TrimSpace(redirectURI) == "" {
		// A code requested with one redirect URI cannot be exchanged with
		// another, and an empty one is always a mismatch. Failing here gives a
		// clear message instead of Meta's generic invalid-code error.
		return Token{}, fmt.Errorf("whatsapp: redirect URI is required")
	}

	short, err := c.exchange(ctx, url.Values{
		"client_id":     {c.appID},
		"client_secret": {c.appSecret},
		"redirect_uri":  {redirectURI},
		"code":          {code},
	})
	if err != nil {
		return Token{}, fmt.Errorf("whatsapp: exchange authorization code: %w", err)
	}

	long, err := c.exchange(ctx, url.Values{
		"client_id":         {c.appID},
		"client_secret":     {c.appSecret},
		"fb_exchange_token": {short.AccessToken},
		"grant_type":        {"fb_exchange_token"},
	})
	if err != nil {
		return Token{}, fmt.Errorf("whatsapp: exchange for long-lived token: %w", err)
	}

	if long.ExpiresIn <= 0 {
		// Without this the row could not be written, so fail loudly rather than
		// inventing an expiry.
		return Token{}, fmt.Errorf("whatsapp: long-lived token response omitted expires_in")
	}

	return Token{
		AccessToken: long.AccessToken,
		ExpiresAt:   time.Now().UTC().Add(time.Duration(long.ExpiresIn) * time.Second),
	}, nil
}

// exchange performs one /oauth/access_token call.
//
// The namespace is unversioned, so it is addressed directly rather than through
// the versioned endpoint helper.
func (c *Client) exchange(ctx context.Context, form url.Values) (tokenResponse, error) {
	var out tokenResponse
	if err := c.get(ctx, "oauth/access_token", form, &out); err != nil {
		return tokenResponse{}, err
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return tokenResponse{}, fmt.Errorf("whatsapp: OAuth response contained no access token")
	}
	return out, nil
}
