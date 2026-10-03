package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is the work factor used for new passwords. 12 is a deliberate
// trade-off: strong enough to make offline cracking expensive, fast enough that
// a login does not feel slow on modest hardware.
const BcryptCost = 12

// Querier is the subset of generated queries the service needs. Declaring it
// here keeps the service testable without a database.
type Querier interface {
	CreateUser(ctx context.Context, arg sqlc.CreateUserParams) (sqlc.User, error)
	GetUserByEmail(ctx context.Context, email string) (sqlc.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
}

// FieldErrors reports per-field validation problems so a form can re-render
// with messages next to the offending inputs.
type FieldErrors map[string]string

func (f FieldErrors) Error() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, f[k]))
	}
	return strings.Join(parts, "; ")
}

// Service holds authentication business logic.
type Service struct {
	queries Querier
	cost    int
	log     *slog.Logger
}

// NewService builds the service. A cost of zero selects BcryptCost.
func NewService(queries Querier, cost int, log *slog.Logger) *Service {
	if cost <= 0 {
		cost = BcryptCost
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{queries: queries, cost: cost, log: log}
}

// Register validates input, hashes the password and creates the account.
func (s *Service) Register(ctx context.Context, email, password string) (User, error) {
	email = NormalizeEmail(email)

	if errs := s.validate(email, password); len(errs) > 0 {
		return User{}, errs
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost)
	if err != nil {
		return User{}, fmt.Errorf("auth: hash password: %w", err)
	}

	row, err := s.queries.CreateUser(ctx, sqlc.CreateUserParams{
		Email:        email,
		PasswordHash: string(hash),
		Plan:         sqlc.UserPlanFree,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("auth: create user: %w", err)
	}

	return userFromRow(row), nil
}

// Authenticate verifies credentials.
//
// Every failure returns ErrInvalidCredentials, including an unknown email, so
// the endpoint cannot be used to discover which addresses have accounts.
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	email = NormalizeEmail(email)
	if email == "" || password == "" {
		return User{}, ErrInvalidCredentials
	}

	row, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend comparable time to a real check so response timing does not
			// reveal whether the account exists.
			s.dummyCompare(password)
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("auth: get user by email: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(password)); err != nil {
		return User{}, ErrInvalidCredentials
	}

	return userFromRow(row), nil
}

// ByID loads a user by id, returning ErrInvalidCredentials when not found so
// callers cannot distinguish a deleted account from a bad request.
func (s *Service) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	if id == uuid.Nil {
		return User{}, ErrInvalidCredentials
	}
	row, err := s.queries.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("auth: get user by id: %w", err)
	}
	return userFromRow(row), nil
}

// dummyCompare burns roughly the same CPU as a real bcrypt comparison.
func (s *Service) dummyCompare(password string) {
	hash, err := bcrypt.GenerateFromPassword([]byte("not-a-real-password"), s.cost)
	if err != nil {
		return
	}
	_ = bcrypt.CompareHashAndPassword(hash, []byte(password))
}

func (s *Service) validate(email, password string) FieldErrors {
	errs := FieldErrors{}
	if err := ValidateEmail(email); err != nil {
		errs["email"] = err.Error()
	}
	if err := ValidatePassword(password); err != nil {
		errs["password"] = err.Error()
	}
	return errs
}

func userFromRow(row sqlc.User) User {
	return User{
		ID:        row.ID,
		Email:     row.Email,
		Plan:      Plan(row.Plan),
		CreatedAt: row.CreatedAt,
	}
}

// isUniqueViolation reports whether err is a Postgres unique constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
