package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// fakeQuerier is an in-memory stand-in for the generated queries.
type fakeQuerier struct {
	users map[string]sqlc.User

	createCalls int
	getCalls    int
	createErr   error
	getErr      error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{users: make(map[string]sqlc.User)}
}

func (f *fakeQuerier) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
	f.createCalls++
	if f.createErr != nil {
		return sqlc.User{}, f.createErr
	}
	if _, exists := f.users[arg.Email]; exists {
		return sqlc.User{}, &uniqueViolation{}
	}
	user := sqlc.User{
		ID:           uuid.New(),
		Email:        arg.Email,
		PasswordHash: arg.PasswordHash,
		Plan:         arg.Plan,
	}
	f.users[arg.Email] = user
	return user, nil
}

func (f *fakeQuerier) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	f.getCalls++
	if f.getErr != nil {
		return sqlc.User{}, f.getErr
	}
	user, ok := f.users[email]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return user, nil
}

func (f *fakeQuerier) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	f.getCalls++
	if f.getErr != nil {
		return sqlc.User{}, f.getErr
	}
	for _, user := range f.users {
		if user.ID == id {
			return user, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

// uniqueViolation mimics a Postgres 23505 error.
type uniqueViolation struct{}

func (*uniqueViolation) Error() string    { return "duplicate key value violates unique constraint" }
func (*uniqueViolation) SQLState() string { return "23505" }

func newTestService(q Querier) *Service {
	return NewService(q, bcrypt.MinCost, nil)
}

func TestRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		email     string
		password  string
		wantErr   bool
		wantField string
		wantErrIs error
	}{
		{name: "valid", email: "user@example.com", password: "correct horse battery", wantErr: false},
		{name: "normalises email", email: "  User@Example.COM ", password: "correct horse battery", wantErr: false},
		{name: "empty email", email: "", password: "correct horse battery", wantErr: true, wantField: "email"},
		{name: "invalid email", email: "not-an-email", password: "correct horse battery", wantErr: true, wantField: "email"},
		{name: "email with display name", email: "User <user@example.com>", password: "correct horse battery", wantErr: true, wantField: "email"},
		{name: "short password", email: "user@example.com", password: "short", wantErr: true, wantField: "password"},
		{name: "password over bcrypt limit", email: "user@example.com", password: string(make([]byte, 73)) + "abcdefghi", wantErr: true, wantField: "password"},
		{name: "both invalid", email: "bad", password: "x", wantErr: true, wantField: "email"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := newFakeQuerier()
			svc := newTestService(q)

			user, err := svc.Register(context.Background(), tc.email, tc.password)

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Register() error = %v, want nil", err)
				}
				if user.Email != NormalizeEmail(tc.email) {
					t.Errorf("user email = %q, want normalised %q", user.Email, NormalizeEmail(tc.email))
				}
				if user.Plan != PlanFree {
					t.Errorf("user plan = %q, want %q", user.Plan, PlanFree)
				}
				if user.ID == uuid.Nil {
					t.Error("user ID is nil")
				}
				return
			}

			if err == nil {
				t.Fatal("Register() error = nil, want error")
			}
			var fieldErrs FieldErrors
			if !errors.As(err, &fieldErrs) {
				t.Fatalf("Register() error = %v, want FieldErrors", err)
			}
			if tc.wantField != "" && fieldErrs[tc.wantField] == "" {
				t.Errorf("FieldErrors = %v, want an entry for %q", fieldErrs, tc.wantField)
			}
			if q.createCalls != 0 {
				t.Errorf("CreateUser called %d times on invalid input, want 0", q.createCalls)
			}
		})
	}
}

func TestRegisterHashesPassword(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	svc := newTestService(q)

	const password = "correct horse battery"
	if _, err := svc.Register(context.Background(), "user@example.com", password); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	stored := q.users["user@example.com"]
	if stored.PasswordHash == password {
		t.Fatal("password was stored in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte(password)); err != nil {
		t.Errorf("stored hash does not verify against the password: %v", err)
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	svc := newTestService(q)

	const password = "correct horse battery"
	if _, err := svc.Register(context.Background(), "user@example.com", password); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}

	_, err := svc.Register(context.Background(), "USER@example.com", password)
	if !errors.Is(err, ErrEmailTaken) {
		t.Errorf("Register() error = %v, want ErrEmailTaken", err)
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (*fakeQuerier, *Service) {
		t.Helper()
		q := newFakeQuerier()
		svc := newTestService(q)
		if _, err := svc.Register(context.Background(), "user@example.com", "correct horse battery"); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		return q, svc
	}

	tests := []struct {
		name     string
		email    string
		password string
		wantErr  bool
	}{
		{name: "valid credentials", email: "user@example.com", password: "correct horse battery", wantErr: false},
		{name: "email normalised", email: "  User@EXAMPLE.com ", password: "correct horse battery", wantErr: false},
		{name: "wrong password", email: "user@example.com", password: "wrong password", wantErr: true},
		{name: "unknown email", email: "nobody@example.com", password: "correct horse battery", wantErr: true},
		{name: "empty password", email: "user@example.com", password: "", wantErr: true},
		{name: "empty email", email: "", password: "correct horse battery", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, svc := setup(t)

			user, err := svc.Authenticate(context.Background(), tc.email, tc.password)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCredentials) {
					t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
				}
				// The message must not reveal whether the account exists.
				if err != nil && err.Error() != "invalid email or password" {
					t.Errorf("error message = %q, want a non-revealing message", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate() error = %v, want nil", err)
			}
			if user.Email != "user@example.com" {
				t.Errorf("user email = %q, want user@example.com", user.Email)
			}
		})
	}
}

// A missing account must not be distinguishable from a wrong password, so the
// service must still perform a bcrypt comparison.
func TestAuthenticateUnknownEmailStillCompares(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	svc := newTestService(q)

	if _, err := svc.Authenticate(context.Background(), "nobody@example.com", "some password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
	// The only query an unknown email should trigger is the lookup.
	if q.getCalls != 1 {
		t.Errorf("GetUserByEmail called %d times, want 1", q.getCalls)
	}
}

func TestByID(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	svc := newTestService(q)

	created, err := svc.Register(context.Background(), "user@example.com", "correct horse battery")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, err := svc.ByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("ByID() error = %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ByID() id = %s, want %s", got.ID, created.ID)
	}

	if _, err := svc.ByID(context.Background(), uuid.New()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("ByID(unknown) error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.ByID(context.Background(), uuid.Nil); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("ByID(nil) error = %v, want ErrInvalidCredentials", err)
	}
}

func TestFieldErrorsMessageIsStable(t *testing.T) {
	t.Parallel()

	errs := FieldErrors{"password": "too short", "email": "invalid"}
	// Sorted so the message does not depend on map iteration order.
	if got := errs.Error(); got != "email: invalid; password: too short" {
		t.Errorf("Error() = %q, want a deterministic sorted message", got)
	}
}
