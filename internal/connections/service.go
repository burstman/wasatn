package connections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/burstman/wasatn/internal/cryptox"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the database surface the service needs. It is an interface so the
// flow can be unit tested without Postgres, while production passes the
// generated sqlc.Queries.
type Querier interface {
	UpsertConnection(ctx context.Context, arg sqlc.UpsertConnectionParams) (sqlc.Connection, error)
	UpsertPendingConnection(ctx context.Context, arg sqlc.UpsertPendingConnectionParams) (sqlc.Connection, error)
	DeletePendingConnectionForAccount(ctx context.Context, arg sqlc.DeletePendingConnectionForAccountParams) (int64, error)
	GetPendingConnectionByUserAndWABA(ctx context.Context, arg sqlc.GetPendingConnectionByUserAndWABAParams) (sqlc.Connection, error)
	GetConnectionByIDForUser(ctx context.Context, arg sqlc.GetConnectionByIDForUserParams) (sqlc.Connection, error)
	ListConnectionsByUser(ctx context.Context, userID uuid.UUID) ([]sqlc.Connection, error)
	CountConnectionsByUser(ctx context.Context, userID uuid.UUID) (int64, error)
	DisconnectOwnedConnectionAndPauseCampaigns(ctx context.Context, arg sqlc.DisconnectOwnedConnectionAndPauseCampaignsParams) (sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow, error)
}

// Graph is the slice of the Meta API the signup flow needs. It is an interface so
// the flow can be tested without reaching the network.
type Graph interface {
	ExchangeCode(ctx context.Context, code, redirectURI string) (whatsapp.Token, error)
	BusinessAccounts(ctx context.Context, token string) ([]whatsapp.BusinessAccount, error)
	PhoneNumbers(ctx context.Context, wabaID, token string) ([]whatsapp.PhoneNumber, error)
	SubscribeApp(ctx context.Context, wabaID, token string) error
}

// Service stores and lists WhatsApp connections.
type Service struct {
	queries Querier
	graph   Graph
	cipher  *cryptox.Cipher
	log     *slog.Logger
}

// NewService wires the connection service. The cipher protects access tokens at
// rest, so a nil one is rejected rather than silently storing plaintext.
func NewService(queries Querier, graph Graph, cipher *cryptox.Cipher, log *slog.Logger) *Service {
	return &Service{
		queries: queries,
		graph:   graph,
		cipher:  cipher,
		log:     log.With("component", "connections"),
	}
}

// Connect runs one Embedded Signup exchange and stores every number it unlocked.
//
// The sequence is Meta's: trade the browser's code for a long-lived token, list
// the WhatsApp Business Accounts that token reaches, subscribe the app to each
// account's webhooks, then record one connection per phone number.
//
// Nothing is written until the webhooks are subscribed, because a connection
// row without working webhooks would send messages that are never tracked.
//
// Rows are written one at a time rather than in a transaction, so an account
// that fails partway leaves the accounts already stored in place. That is safe
// because every write is an upsert keyed by the phone number: pressing the
// button again converges on the same set of rows.
func (s *Service) Connect(ctx context.Context, userID uuid.UUID, code, redirectURI string) (Result, error) {
	if userID == uuid.Nil {
		return Result{}, errors.New("connections: a signed-in user is required")
	}
	if s.graph == nil {
		// An app booted without Meta credentials has no client to exchange the
		// code with. Saying so plainly beats a nil dereference on the first
		// customer who clicks the button.
		return Result{}, ErrSignupNotConfigured
	}

	token, err := s.graph.ExchangeCode(ctx, code, redirectURI)
	if err != nil {
		return Result{}, err
	}

	accounts, err := s.graph.BusinessAccounts(ctx, token.AccessToken)
	if err != nil {
		return Result{}, err
	}
	if len(accounts) == 0 {
		// A successful code exchange with no accounts means the customer
		// authorised WasaTN without picking a WhatsApp Business Account.
		return Result{}, ErrNothingConnected
	}

	result := Result{Accounts: len(accounts)}
	for _, account := range accounts {
		// The per-account token from /me/accounts is scoped to one WABA, which
		// is what keeps one customer's account updates from touching another's.
		// BusinessAccounts falls back to the long-lived token when Meta omits it.
		accountToken := account.AccessToken
		if accountToken == "" {
			accountToken = token.AccessToken
		}

		if err := s.graph.SubscribeApp(ctx, account.ID, accountToken); err != nil {
			return Result{}, err
		}

		numbers, err := s.graph.PhoneNumbers(ctx, account.ID, accountToken)
		if err != nil {
			return Result{}, err
		}

		rows, err := s.storeAccount(ctx, userID, account, token, numbers)
		if err != nil {
			return Result{}, err
		}
		result.Connections = append(result.Connections, rows...)
		for _, row := range rows {
			if row.Status == StatusPendingPhone {
				result.PendingPhone = true
			}
		}
	}

	if len(result.Connections) == 0 {
		// Meta reported accounts but nothing could be recorded, which means every
		// number failed validation rather than that the customer shared nothing.
		return Result{}, fmt.Errorf("connections: no phone numbers could be stored for %d account(s)", len(accounts))
	}

	s.log.InfoContext(ctx, "connected whatsapp accounts",
		"user_id", userID,
		"accounts", result.Accounts,
		"connections", len(result.Connections),
		"pending_phone", result.PendingPhone,
	)
	return result, nil
}

// storeAccount writes the rows for one WhatsApp Business Account.
func (s *Service) storeAccount(ctx context.Context, userID uuid.UUID, account whatsapp.BusinessAccount, token whatsapp.Token, numbers []whatsapp.PhoneNumber) ([]sqlc.Connection, error) {
	// One row per phone number: a WhatsApp Business Account can own several, and
	// a customer connects all of them at once.
	var rows []sqlc.Connection

	for i := range numbers {
		number := numbers[i]

		// Embedded Signup v4 lets a business share an account before a number is
		// verified. WasaTN only sends from a business's own number, so a shared
		// display number is recorded with the number kept for reference but
		// left pending, rather than presented as ready to send from.
		status := StatusActive
		if !number.IsOfficialBusinessAccount {
			status = StatusPendingPhone
		}

		params, err := upsertParams(userID, account, token.ExpiresAt, &number, status)
		if err != nil {
			// A number Meta returned that is not readable as E.164 must not fail
			// the whole signup, and must never be guessed at.
			s.log.WarnContext(ctx, "skipping unusable phone number",
				"messaging_account_id", account.ID,
				"phone_number_id", number.ID,
				"err", err.Error(),
			)
			continue
		}

		row, err := s.upsertEncrypted(ctx, params, token.AccessToken)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}

	if len(rows) > 0 {
		// The account has numbers now, so any placeholder from an earlier signup
		// is stale. Leaving it behind would show the customer both "Needs a
		// number" and the number they just connected.
		if _, err := s.queries.DeletePendingConnectionForAccount(ctx, sqlc.DeletePendingConnectionForAccountParams{
			UserID:             userID,
			MessagingAccountID: account.ID,
		}); err != nil {
			return nil, fmt.Errorf("connections: clear pending connection: %w", err)
		}
		return rows, nil
	}

	// No number was storable. If a placeholder already exists for this account,
	// leave it alone: a reconnect that briefly reports no numbers must not reset
	// the token or the expiry on a row the customer can see.
	existing, err := s.queries.GetPendingConnectionByUserAndWABA(ctx, sqlc.GetPendingConnectionByUserAndWABAParams{
		UserID:             userID,
		MessagingAccountID: account.ID,
	})
	switch {
	case err == nil:
		return []sqlc.Connection{existing}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Genuinely new account with no number: record it as pending.
	default:
		return nil, fmt.Errorf("connections: look up pending connection: %w", err)
	}

	row, err := s.upsertPendingEncrypted(ctx, sqlc.UpsertPendingConnectionParams{
		UserID:             userID,
		MessagingAccountID: account.ID,
		TokenExpiresAt:     token.ExpiresAt,
		Status:             StatusPendingPhone,
	}, token.AccessToken)
	if err != nil {
		return nil, err
	}
	return []sqlc.Connection{row}, nil
}

// upsertPendingEncrypted encrypts the access token into a pending row.
//
// It is separate from upsertEncrypted because the two rows have different conflict
// targets: a pending row has no phone number to be identified by.
func (s *Service) upsertPendingEncrypted(ctx context.Context, params sqlc.UpsertPendingConnectionParams, accessToken string) (sqlc.Connection, error) {
	encrypted, err := s.cipher.EncryptString(accessToken)
	if err != nil {
		return sqlc.Connection{}, fmt.Errorf("connections: encrypt access token: %w", err)
	}
	params.AccessTokenEncrypted = encrypted

	row, err := s.queries.UpsertPendingConnection(ctx, params)
	if err != nil {
		return sqlc.Connection{}, fmt.Errorf("connections: store pending connection: %w", err)
	}
	return row, nil
}

// upsertEncrypted encrypts the access token into the row and writes it.
//
// Encryption happens per row rather than once per signup. AES-GCM is
// non-deterministic, so a shared ciphertext would make it obvious that every
// connection from one signup carries the same token.
func (s *Service) upsertEncrypted(ctx context.Context, params sqlc.UpsertConnectionParams, accessToken string) (sqlc.Connection, error) {
	encrypted, err := s.cipher.EncryptString(accessToken)
	if err != nil {
		return sqlc.Connection{}, fmt.Errorf("connections: encrypt access token: %w", err)
	}
	params.AccessTokenEncrypted = encrypted

	// upsert writes one row, translating the global phone-number ownership index
	// into a typed error.
	row, err := s.queries.UpsertConnection(ctx, params)
	if err == nil {
		return row, nil
	}

	if isPhoneNumberOwnedByAnother(connErr(err)) {
		// The masked number is safe in a log; the full one is not.
		s.log.WarnContext(ctx, "phone number already connected elsewhere",
			"user_id", params.UserID,
			"messaging_account_id", params.MessagingAccountID,
			"phone_number_id", params.PhoneNumberID,
		)
		return sqlc.Connection{}, ErrNumberOwnedByAnotherAccount
	}
	return sqlc.Connection{}, fmt.Errorf("connections: store connection: %w", err)
}

// List returns every connection the user owns.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]sqlc.Connection, error) {
	if userID == uuid.Nil {
		return nil, errors.New("connections: a signed-in user is required")
	}
	rows, err := s.queries.ListConnectionsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("connections: list connections: %w", err)
	}
	return rows, nil
}

// Count returns how many connections the user owns, for the dashboard.
func (s *Service) Count(ctx context.Context, userID uuid.UUID) (int, error) {
	if userID == uuid.Nil {
		return 0, errors.New("connections: a signed-in user is required")
	}
	n, err := s.queries.CountConnectionsByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("connections: count connections: %w", err)
	}
	return int(n), nil
}

// Get returns one connection owned by the user, or ErrNotFound.
func (s *Service) Get(ctx context.Context, userID, connectionID uuid.UUID) (sqlc.Connection, error) {
	if userID == uuid.Nil || connectionID == uuid.Nil {
		return sqlc.Connection{}, ErrNotFound
	}
	row, err := s.queries.GetConnectionByIDForUser(ctx, sqlc.GetConnectionByIDForUserParams{
		ID:     connectionID,
		UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Connection{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Connection{}, fmt.Errorf("connections: get connection: %w", err)
	}
	return row, nil
}

// Disconnect marks a connection disconnected and pauses its running campaigns.
//
// Reconnecting is the fix for a disconnected or expired connection: the signup
// button writes a fresh token over the same row.
func (s *Service) Disconnect(ctx context.Context, userID, connectionID uuid.UUID) error {
	if userID == uuid.Nil || connectionID == uuid.Nil {
		return ErrNotFound
	}
	result, err := s.queries.DisconnectOwnedConnectionAndPauseCampaigns(ctx,
		sqlc.DisconnectOwnedConnectionAndPauseCampaignsParams{
			ID:     connectionID,
			UserID: userID,
		})
	if err != nil {
		return fmt.Errorf("connections: disconnect: %w", err)
	}
	if !result.Changed {
		// Either the id belongs to nobody, or it was already disconnected. The
		// customer is told it is already disconnected, which is both true and
		// reveals nothing about other tenants.
		return ErrAlreadyDisconnected
	}

	s.log.InfoContext(ctx, "connection disconnected",
		"user_id", userID,
		"connection_id", connectionID,
		"paused_campaigns", result.PausedCampaigns,
	)
	return nil
}

// Token decrypts a connection's access token for an API call.
//
// The ciphertext never leaves the database layer, and the caller must not log
// the result.
func (s *Service) Token(ctx context.Context, row sqlc.Connection) (string, error) {
	token, err := s.cipher.DecryptString(row.AccessTokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("connections: decrypt access token: %w", err)
	}
	if token == "" {
		return "", errors.New("connections: stored access token is empty")
	}
	return token, nil
}

// connErr extracts the *pgconn.PgError behind err, if there is one.
func connErr(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

// isPhoneNumberOwnedByAnother matches the violation raised by the global
// unique index on connections.phone_number_id, which exists so one WhatsApp
// number can never belong to two tenants.
func isPhoneNumberOwnedByAnother(pgErr *pgconn.PgError) bool {
	return pgErr != nil &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "connections_phone_number_id_key"
}
