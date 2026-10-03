package connections

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/burstman/wasatn/internal/cryptox"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// testUser is the signed-in customer in these tests.
var testUser = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func testCipher(t *testing.T) *cryptox.Cipher {
	t.Helper()
	cipher, err := cryptox.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeGraph is a Meta API stub.
type fakeGraph struct {
	token    whatsapp.Token
	accounts []whatsapp.BusinessAccount
	numbers  map[string][]whatsapp.PhoneNumber

	exchangedCode string
	exchangedURI  string
	subscribed    []string
	calls         []string

	exchangeErr  error
	accountsErr  error
	numbersErr   error
	subscribeErr error
}

func (f *fakeGraph) ExchangeCode(_ context.Context, code, redirectURI string) (whatsapp.Token, error) {
	f.calls = append(f.calls, "ExchangeCode")
	f.exchangedCode = code
	f.exchangedURI = redirectURI
	if f.exchangeErr != nil {
		return whatsapp.Token{}, f.exchangeErr
	}
	return f.token, nil
}

func (f *fakeGraph) BusinessAccounts(_ context.Context, token string) ([]whatsapp.BusinessAccount, error) {
	f.calls = append(f.calls, "BusinessAccounts")
	if f.accountsErr != nil {
		return nil, f.accountsErr
	}
	return f.accounts, nil
}

func (f *fakeGraph) PhoneNumbers(_ context.Context, wabaID, _ string) ([]whatsapp.PhoneNumber, error) {
	f.calls = append(f.calls, "PhoneNumbers:"+wabaID)
	if f.numbersErr != nil {
		return nil, f.numbersErr
	}
	return f.numbers[wabaID], nil
}

func (f *fakeGraph) SubscribeApp(_ context.Context, wabaID, _ string) error {
	f.calls = append(f.calls, "SubscribeApp:"+wabaID)
	f.subscribed = append(f.subscribed, wabaID)
	return f.subscribeErr
}

// fakeQueries is a database stub.
type fakeQueries struct {
	upserts     []sqlc.UpsertConnectionParams
	upsertErr   error
	upsertIndex int

	pendingUpserts   []sqlc.UpsertPendingConnectionParams
	pendingErr       error
	pendingDeleteErr error

	deletedPending []string

	existing    map[string]sqlc.Connection
	existingErr error

	list     []sqlc.Connection
	listErr  error
	count    int64
	countErr error

	byID     sqlc.Connection
	byIDErr  error
	getCalls []uuid.UUID

	disconnected  sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow
	disconnectErr error
}

func newFakeQueries() *fakeQueries {
	return &fakeQueries{existing: map[string]sqlc.Connection{}}
}

func (f *fakeQueries) UpsertConnection(_ context.Context, arg sqlc.UpsertConnectionParams) (sqlc.Connection, error) {
	f.upserts = append(f.upserts, arg)
	if f.upsertErr != nil {
		return sqlc.Connection{}, f.upsertErr
	}
	f.upsertIndex++
	return sqlc.Connection{
		ID:                   uuid.New(),
		UserID:               arg.UserID,
		MessagingAccountID:   arg.MessagingAccountID,
		PhoneNumberID:        arg.PhoneNumberID,
		PhoneNumber:          arg.PhoneNumber,
		DisplayName:          arg.DisplayName,
		AccessTokenEncrypted: arg.AccessTokenEncrypted,
		TokenExpiresAt:       arg.TokenExpiresAt,
		QualityRating:        arg.QualityRating,
		MessagingLimit:       arg.MessagingLimit,
		Status:               arg.Status,
	}, nil
}

func (f *fakeQueries) UpsertPendingConnection(_ context.Context, arg sqlc.UpsertPendingConnectionParams) (sqlc.Connection, error) {
	f.pendingUpserts = append(f.pendingUpserts, arg)
	if f.pendingErr != nil {
		return sqlc.Connection{}, f.pendingErr
	}
	return sqlc.Connection{
		ID:                   uuid.New(),
		UserID:               arg.UserID,
		MessagingAccountID:   arg.MessagingAccountID,
		WaacID:               arg.WaacID,
		AccessTokenEncrypted: arg.AccessTokenEncrypted,
		TokenExpiresAt:       arg.TokenExpiresAt,
		QualityRating:        "UNKNOWN",
		Status:               arg.Status,
	}, nil
}

func (f *fakeQueries) DeletePendingConnectionForAccount(_ context.Context, arg sqlc.DeletePendingConnectionForAccountParams) (int64, error) {
	if f.pendingDeleteErr != nil {
		return 0, f.pendingDeleteErr
	}
	f.deletedPending = append(f.deletedPending, arg.MessagingAccountID)
	delete(f.existing, arg.MessagingAccountID)
	return 1, nil
}

func (f *fakeQueries) GetPendingConnectionByUserAndWABA(_ context.Context, arg sqlc.GetPendingConnectionByUserAndWABAParams) (sqlc.Connection, error) {
	if f.existingErr != nil {
		return sqlc.Connection{}, f.existingErr
	}
	row, ok := f.existing[arg.MessagingAccountID]
	if !ok {
		return sqlc.Connection{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeQueries) GetConnectionByIDForUser(_ context.Context, arg sqlc.GetConnectionByIDForUserParams) (sqlc.Connection, error) {
	f.getCalls = append(f.getCalls, arg.ID)
	if f.byIDErr != nil {
		return sqlc.Connection{}, f.byIDErr
	}
	if f.byID.ID == uuid.Nil {
		return sqlc.Connection{}, pgx.ErrNoRows
	}
	return f.byID, nil
}

func (f *fakeQueries) ListConnectionsByUser(_ context.Context, _ uuid.UUID) ([]sqlc.Connection, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeQueries) CountConnectionsByUser(_ context.Context, _ uuid.UUID) (int64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func (f *fakeQueries) DisconnectOwnedConnectionAndPauseCampaigns(_ context.Context, _ sqlc.DisconnectOwnedConnectionAndPauseCampaignsParams) (sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow, error) {
	if f.disconnectErr != nil {
		return sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{}, f.disconnectErr
	}
	return f.disconnected, nil
}

// newService wires a service with stubs and no timestamp stubbing, so tests assert
// on relationships rather than wall-clock values.
func newService(t *testing.T, queries Querier, graph Graph) *Service {
	t.Helper()
	return NewService(queries, graph, testCipher(t), discardLogger())
}

func defaultGraph() *fakeGraph {
	return &fakeGraph{
		token: whatsapp.Token{
			AccessToken: "long-lived-token",
			ExpiresAt:   time.Now().UTC().Add(60 * 24 * time.Hour),
		},
		accounts: []whatsapp.BusinessAccount{
			{ID: "waba-1", Name: "Acme", AccessToken: "waba-token-1"},
		},
		numbers: map[string][]whatsapp.PhoneNumber{
			"waba-1": {
				{
					ID:                        "pn-1",
					DisplayPhoneNumber:        "+1 555-010-9999",
					VerifiedName:              "Acme Support",
					QualityRating:             "GREEN",
					IsOfficialBusinessAccount: true,
					MessagingLimit:            1000,
				},
			},
		},
	}
}

func TestConnectStoresOneRowPerNumber(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.numbers["waba-1"] = append(graph.numbers["waba-1"], whatsapp.PhoneNumber{
		ID:                        "pn-2",
		DisplayPhoneNumber:        "+442079460958",
		IsOfficialBusinessAccount: true,
	})

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "auth-code", "https://wasatn.test/connections")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if len(result.Connections) != 2 {
		t.Fatalf("got %d connections, want 2", len(result.Connections))
	}
	if result.Accounts != 1 {
		t.Fatalf("Accounts = %d, want 1", result.Accounts)
	}
	if result.PendingPhone {
		t.Error("PendingPhone = true, want false when a number was stored")
	}

	if graph.exchangedCode != "auth-code" {
		t.Errorf("exchanged code = %q", graph.exchangedCode)
	}
	if graph.exchangedURI != "https://wasatn.test/connections" {
		t.Errorf("exchanged redirect URI = %q", graph.exchangedURI)
	}
	if len(graph.subscribed) != 1 || graph.subscribed[0] != "waba-1" {
		t.Errorf("subscribed = %v, want the account subscribed", graph.subscribed)
	}
}

func TestConnectStoresTheDocumentedColumns(t *testing.T) {
	queries := newFakeQueries()
	service := newService(t, queries, defaultGraph())

	if _, err := service.Connect(context.Background(), testUser, "code", "https://wasatn.test/connections"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(queries.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(queries.upserts))
	}

	got := queries.upserts[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"user", got.UserID, testUser},
		{"messaging account", got.MessagingAccountID, "waba-1"},
		{"status", got.Status, StatusActive},
		{"quality", got.QualityRating, "GREEN"},
		{"messaging limit", got.MessagingLimit, int32(1000)},
		{"display name", got.DisplayName, "Acme Support"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if got.PhoneNumberID == nil || *got.PhoneNumberID != "pn-1" {
		t.Errorf("phone_number_id = %v", got.PhoneNumberID)
	}
	if got.PhoneNumber == nil || *got.PhoneNumber != "+15550109999" {
		t.Errorf("phone_number = %v, want normalised E.164", got.PhoneNumber)
	}
	if got.TokenExpiresAt.IsZero() {
		t.Error("token_expires_at was not set")
	}
}

// The token is a customer credential: it must be encrypted at rest, and it must
// never appear in a log line.
func TestConnectEncryptsTheAccessToken(t *testing.T) {
	queries := newFakeQueries()
	cipher := testCipher(t)
	service := NewService(queries, defaultGraph(), cipher, discardLogger())

	if _, err := service.Connect(context.Background(), testUser, "code", "https://wasatn.test/connections"); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ciphertext := queries.upserts[0].AccessTokenEncrypted
	if strings.Contains(string(ciphertext), "long-lived-token") {
		t.Fatal("the access token was stored in plaintext")
	}
	if len(ciphertext) == 0 {
		t.Fatal("no ciphertext was stored")
	}

	plaintext, err := cipher.DecryptString(ciphertext)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if plaintext != "long-lived-token" {
		t.Fatalf("round trip = %q", plaintext)
	}
}

// One ciphertext is reused for every row of a signup. AES-GCM is
// non-deterministic, so reusing it would leak that the rows share a token.
func TestConnectDoesNotReuseOneCiphertextAcrossRows(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.numbers["waba-1"] = append(graph.numbers["waba-1"], whatsapp.PhoneNumber{
		ID:                        "pn-2",
		DisplayPhoneNumber:        "+442079460958",
		IsOfficialBusinessAccount: true,
	})

	service := newService(t, queries, graph)
	if _, err := service.Connect(context.Background(), testUser, "code", "https://wasatn.test/connections"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(queries.upserts) != 2 {
		t.Fatalf("got %d upserts, want 2", len(queries.upserts))
	}
	if len(queries.pendingUpserts) != 0 {
		t.Error("a placeholder was written for an account that has numbers")
	}

	first := string(queries.upserts[0].AccessTokenEncrypted)
	second := string(queries.upserts[1].AccessTokenEncrypted)
	if first == second {
		t.Fatal("both rows share one ciphertext")
	}
}

func TestConnectRequiresASignedInUser(t *testing.T) {
	service := newService(t, newFakeQueries(), defaultGraph())
	if _, err := service.Connect(context.Background(), uuid.Nil, "code", "https://x.test/cb"); err == nil {
		t.Fatal("expected an error for an anonymous user")
	}
}

// An app booted without Meta credentials has no client to exchange with; saying
// so beats a nil dereference on the first click.
func TestConnectReportsMissingMetaConfiguration(t *testing.T) {
	service := newService(t, newFakeQueries(), nil)
	_, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if !errors.Is(err, ErrSignupNotConfigured) {
		t.Fatalf("error = %v, want ErrSignupNotConfigured", err)
	}
}

func TestConnectWhenMetaSharesNoAccount(t *testing.T) {
	graph := defaultGraph()
	graph.accounts = nil

	service := newService(t, newFakeQueries(), graph)
	_, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if !errors.Is(err, ErrNothingConnected) {
		t.Fatalf("error = %v, want ErrNothingConnected", err)
	}
}

// Embedded Signup v4 lets an account finish with no verified number, so a first
// onboarding is recorded as pending rather than treated as a failure.
func TestConnectRecordsPendingPhoneForAnAccountWithNoNumbers(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.numbers["waba-1"] = nil

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if !result.PendingPhone {
		t.Error("PendingPhone = false, want true")
	}
	if len(queries.pendingUpserts) != 1 {
		t.Fatalf("got %d pending upserts, want 1", len(queries.pendingUpserts))
	}
	if len(queries.upserts) != 0 {
		t.Fatal("a number row was written for an account with no numbers")
	}
	row := queries.pendingUpserts[0]
	if row.Status != StatusPendingPhone {
		t.Errorf("status = %q, want pending_phone", row.Status)
	}
	if row.AccessTokenEncrypted == nil {
		t.Error("the pending row has no token")
	}
}

// A reconnect that briefly reports no numbers must not blank a working row.
func TestConnectLeavesAnExistingConnectionAloneWhenNoNumbersAreReported(t *testing.T) {
	queries := newFakeQueries()
	queries.existing["waba-1"] = sqlc.Connection{
		ID:                 uuid.New(),
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		PhoneNumberID:      ptr("pn-1"),
		PhoneNumber:        ptr("+15550109999"),
		Status:             StatusActive,
	}
	graph := defaultGraph()
	graph.numbers["waba-1"] = nil

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(queries.upserts) != 0 {
		t.Fatalf("the existing row was overwritten: %+v", queries.upserts)
	}
	if len(result.Connections) != 1 {
		t.Fatalf("got %d connections, want the existing one", len(result.Connections))
	}
	if result.PendingPhone {
		t.Error("PendingPhone = true, want false for an untouched active row")
	}
}

// WasaTN only sends from a business's own number, so a shared display number is
// recorded but not marked ready to send.
func TestConnectKeepsANonOfficialNumberPending(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.numbers["waba-1"] = []whatsapp.PhoneNumber{{
		ID:                        "pn-shared",
		DisplayPhoneNumber:        "+15550109999",
		IsOfficialBusinessAccount: false,
	}}

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if len(queries.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(queries.upserts))
	}
	if queries.upserts[0].Status != StatusPendingPhone {
		t.Errorf("status = %q, want pending_phone", queries.upserts[0].Status)
	}
	if result.PendingPhone != true {
		t.Error("PendingPhone = false, want true")
	}
	// The number is still kept, so the customer can see which number Meta shared.
	if queries.upserts[0].PhoneNumber == nil {
		t.Error("the phone number was discarded")
	}
}

// A number arriving later replaces the placeholder: the customer must not be
// left with both "Needs a number" and the number they just connected.
func TestConnectClearsThePendingRowOnceTheAccountHasANumber(t *testing.T) {
	queries := newFakeQueries()
	queries.existing["waba-1"] = sqlc.Connection{
		ID:                 uuid.New(),
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		Status:             StatusPendingPhone,
	}

	service := newService(t, queries, defaultGraph())
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if len(queries.deletedPending) != 1 || queries.deletedPending[0] != "waba-1" {
		t.Errorf("deleted pending rows for %v, want waba-1", queries.deletedPending)
	}
	if len(queries.upserts) != 1 {
		t.Fatalf("got %d number rows, want 1", len(queries.upserts))
	}
	if result.PendingPhone {
		t.Error("PendingPhone = true, want false once a number is stored")
	}
}

// Reconnecting with no numbers must not reset the token on a placeholder the
// customer can already see.
func TestConnectLeavesThePendingRowAloneWhenNoNumbersAreReported(t *testing.T) {
	queries := newFakeQueries()
	queries.existing["waba-1"] = sqlc.Connection{
		ID:                 uuid.New(),
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		Status:             StatusPendingPhone,
	}
	graph := defaultGraph()
	graph.numbers["waba-1"] = nil

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if len(queries.pendingUpserts) != 0 {
		t.Fatal("the existing placeholder was rewritten")
	}
	if len(queries.deletedPending) != 0 {
		t.Fatal("the placeholder was deleted even though there is no number to replace it with")
	}
	if len(result.Connections) != 1 {
		t.Fatalf("got %d connections, want the placeholder", len(result.Connections))
	}
}

// A placeholder exists per account, not per number, so a failed delete must not
// be swallowed: it would leave the customer with a contradictory page.
func TestConnectFailsWhenTheStalePlaceholderCannotBeRemoved(t *testing.T) {
	queries := newFakeQueries()
	queries.pendingDeleteErr = errors.New("permission denied")

	service := newService(t, queries, defaultGraph())
	if _, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb"); err == nil {
		t.Fatal("expected an error")
	}
}

// A number Meta returned that cannot be read as E.164 must be skipped rather than
// guessed at, and must not fail the whole signup.
func TestConnectSkipsANumberThatIsNotE164(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.numbers["waba-1"] = []whatsapp.PhoneNumber{
		{ID: "pn-bad", DisplayPhoneNumber: "unknown", IsOfficialBusinessAccount: true},
		{ID: "pn-good", DisplayPhoneNumber: "+15550109999", IsOfficialBusinessAccount: true},
	}

	service := newService(t, queries, graph)
	result, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if len(queries.upserts) != 1 {
		t.Fatalf("got %d upserts, want only the usable number", len(queries.upserts))
	}
	if queries.upserts[0].PhoneNumberID == nil || *queries.upserts[0].PhoneNumberID != "pn-good" {
		t.Errorf("stored the wrong number: %+v", queries.upserts[0])
	}
	if len(result.Connections) != 1 {
		t.Errorf("got %d connections, want 1", len(result.Connections))
	}
}

// A webhook subscription is what makes delivery tracking work, so a failure to
// subscribe must not be recorded as a working connection.
func TestConnectFailsWhenTheAppCannotSubscribe(t *testing.T) {
	queries := newFakeQueries()
	graph := defaultGraph()
	graph.subscribeErr = errors.New("permissions error")

	service := newService(t, queries, graph)
	if _, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb"); err == nil {
		t.Fatal("expected an error")
	}
	if len(queries.upserts) != 0 {
		t.Fatal("a connection was stored despite the failed subscription")
	}
}

func TestConnectPropagatesTokenExchangeFailure(t *testing.T) {
	graph := defaultGraph()
	graph.exchangeErr = &whatsapp.APIError{Message: "Invalid code", Code: 100}

	service := newService(t, newFakeQueries(), graph)
	_, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")

	var apiErr *whatsapp.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *whatsapp.APIError", err)
	}
	if apiErr.Code != 100 {
		t.Fatalf("Code = %d", apiErr.Code)
	}
}

// The global unique index on phone_number_id is what stops one number belonging
// to two tenants; its violation has to surface as a conflict, not a 500.
func TestConnectMapsThePhoneNumberOwnershipConflict(t *testing.T) {
	queries := newFakeQueries()
	queries.upsertErr = &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "connections_phone_number_id_key",
	}

	service := newService(t, queries, defaultGraph())
	_, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if !errors.Is(err, ErrNumberOwnedByAnotherAccount) {
		t.Fatalf("error = %v, want ErrNumberOwnedByAnotherAccount", err)
	}
}

func TestConnectDoesNotSwallowOtherConstraintViolations(t *testing.T) {
	queries := newFakeQueries()
	queries.upsertErr = &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "connections_phone_e164",
	}

	service := newService(t, queries, defaultGraph())
	_, err := service.Connect(context.Background(), testUser, "code", "https://x.test/cb")
	if errors.Is(err, ErrNumberOwnedByAnotherAccount) {
		t.Fatal("an unrelated constraint violation was reported as an ownership conflict")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetRejectsAnonymousAndUnknownIDs(t *testing.T) {
	service := newService(t, newFakeQueries(), defaultGraph())

	if _, err := service.Get(context.Background(), uuid.Nil, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("anonymous user error = %v, want ErrNotFound", err)
	}
	if _, err := service.Get(context.Background(), testUser, uuid.Nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("zero id error = %v, want ErrNotFound", err)
	}
	if _, err := service.Get(context.Background(), testUser, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id error = %v, want ErrNotFound", err)
	}
}

// A connection id that belongs to another tenant must be indistinguishable from
// one that does not exist.
func TestGetDoesNotLeakAnotherTenantsConnection(t *testing.T) {
	queries := newFakeQueries()
	queries.byIDErr = pgx.ErrNoRows

	service := newService(t, queries, defaultGraph())
	_, err := service.Get(context.Background(), testUser, uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if len(queries.getCalls) != 1 {
		t.Fatalf("expected the ownership check to happen in the query")
	}
}

func TestListAndCountRequireASignedInUser(t *testing.T) {
	service := newService(t, newFakeQueries(), defaultGraph())

	if _, err := service.List(context.Background(), uuid.Nil); err == nil {
		t.Error("List accepted an anonymous user")
	}
	if _, err := service.Count(context.Background(), uuid.Nil); err == nil {
		t.Error("Count accepted an anonymous user")
	}
}

func TestListWrapsDatabaseFailures(t *testing.T) {
	queries := newFakeQueries()
	queries.listErr = errors.New("connection refused")

	service := newService(t, queries, defaultGraph())
	_, err := service.List(context.Background(), testUser)
	if err == nil || !strings.Contains(err.Error(), "list connections") {
		t.Fatalf("error = %v, want it to name the operation", err)
	}
}

func TestDisconnectReportsSuccess(t *testing.T) {
	queries := newFakeQueries()
	queries.disconnected = sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{Changed: true, PausedCampaigns: 3}

	service := newService(t, queries, defaultGraph())
	if err := service.Disconnect(context.Background(), testUser, uuid.New()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

// Re-running a disconnect, or disconnecting one Meta already disconnected, is a
// no-op rather than an error the page has to explain.
func TestDisconnectIsIdempotent(t *testing.T) {
	queries := newFakeQueries()
	queries.disconnected = sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{Changed: false}

	service := newService(t, queries, defaultGraph())
	err := service.Disconnect(context.Background(), testUser, uuid.New())
	if !errors.Is(err, ErrAlreadyDisconnected) {
		t.Fatalf("error = %v, want ErrAlreadyDisconnected", err)
	}
}

func TestDisconnectRejectsAnonymousAndUnknownIDs(t *testing.T) {
	service := newService(t, newFakeQueries(), defaultGraph())

	if err := service.Disconnect(context.Background(), uuid.Nil, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("anonymous user error = %v, want ErrNotFound", err)
	}
	if err := service.Disconnect(context.Background(), testUser, uuid.Nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("zero id error = %v, want ErrNotFound", err)
	}
}

func TestTokenDecryptsAndRejectsGarbage(t *testing.T) {
	cipher := testCipher(t)
	service := NewService(newFakeQueries(), defaultGraph(), cipher, discardLogger())

	ciphertext, err := cipher.EncryptString("token-value")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	got, err := service.Token(context.Background(), sqlc.Connection{AccessTokenEncrypted: ciphertext})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "token-value" {
		t.Fatalf("Token = %q", got)
	}

	if _, err := service.Token(context.Background(), sqlc.Connection{AccessTokenEncrypted: []byte("garbage")}); err == nil {
		t.Fatal("expected an error for an undecryptable token")
	}
	if _, err := service.Token(context.Background(), sqlc.Connection{AccessTokenEncrypted: nil}); err == nil {
		t.Fatal("expected an error for a missing token")
	}
}

func ptr(s string) *string { return &s }
