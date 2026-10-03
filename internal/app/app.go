// Package app wires configuration, the database, sessions and routes into a
// single http.Handler.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/anthdm/superkit/kit"
	wasatn "github.com/burstman/wasatn"
	"github.com/burstman/wasatn/internal/auth"
	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/connections"
	"github.com/burstman/wasatn/internal/cryptox"
	"github.com/burstman/wasatn/internal/db"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/httpx"
	"github.com/burstman/wasatn/internal/jobs"
	"github.com/burstman/wasatn/internal/web"
	"github.com/burstman/wasatn/internal/webhooks"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthRateLimit caps credential submissions per client address, which blunts
// password guessing.
const (
	AuthRateLimitAttempts = 10
	AuthRateLimitWindow   = time.Minute
)

// App holds the dependencies shared by the HTTP handlers.
type App struct {
	Config      *config.Config
	Log         *slog.Logger
	Pool        *pgxpool.Pool
	Queries     *sqlc.Queries
	Session     *auth.Session
	CSRF        *httpx.CSRF
	Auth        *auth.Handlers
	Connections *connections.Handlers
	Jobs        *jobs.Client
	Webhooks    *webhooks.Handler

	staticFS  http.FileSystem
	authLimit *httpx.Limiter
}

// New builds the application. The River client is created for enqueueing only;
// the worker process calls jobs.Run separately.
func New(cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) (*App, error) {
	staticFS, err := buildStaticFS()
	if err != nil {
		return nil, err
	}

	session := auth.NewSession(
		pgxstore.New(pool),
		cfg.SessionCookieName,
		cfg.SessionLifetime,
		cfg.SessionIdleTimeout,
		cfg.Production(),
	)

	csrf := httpx.NewCSRF(session.Manager(), cfg.Production())
	queries := db.NewQueries(pool)
	service := auth.NewService(queries, auth.BcryptCost, log)

	// Access tokens are encrypted at rest, so a database leak cannot be turned
	// into a customer sending from their own number.
	cipher, err := cryptox.NewCipher(cfg.TokenKey)
	if err != nil {
		return nil, err
	}

	// The Graph client is an interface field so an app without Meta credentials
	// still boots: the webhook keeps serving and the connections page explains
	// what is missing. Declaring the interface first, rather than holding a
	// *whatsapp.Client that may be nil, keeps the "not configured" case a real
	// nil check instead of a panic on first use.
	var graph connections.Graph
	if client, clientErr := whatsapp.NewClient(whatsapp.Config{
		AppID:        cfg.Meta.AppID,
		AppSecret:    cfg.Meta.AppSecret,
		GraphVersion: cfg.Meta.Version(),
	}); clientErr != nil {
		// An unconfigured signup must not stop the webhook serving.
		log.WarnContext(context.Background(), "embedded signup disabled", "err", clientErr.Error())
	} else {
		graph = client
	}

	jobClient, err := jobs.NewClient(pool, &cfg.River, nil, log.With("component", "river"))
	if err != nil {
		return nil, err
	}

	app := &App{
		Config:    cfg,
		Log:       log,
		Pool:      pool,
		Queries:   queries,
		Session:   session,
		CSRF:      csrf,
		Auth:      auth.NewHandlers(service, session, csrf, log.With("component", "auth")),
		Jobs:      jobClient,
		Webhooks:  webhooks.New(queries, cfg.Meta.AppSecret, cfg.Meta.VerifyToken, log),
		staticFS:  staticFS,
		authLimit: httpx.NewLimiter(AuthRateLimitAttempts, AuthRateLimitWindow),
	}

	// The service is built even when the Graph client is nil: listing and
	// disconnecting connections still work, and Connect reports the missing
	// configuration rather than panicking.
	connectionService := connections.NewService(queries, graph, cipher, log.With("component", "connections"))
	app.Connections = connections.NewHandlers(connectionService, session, connections.SignupConfig{
		AppID:       cfg.Meta.AppID,
		ConfigID:    cfg.Meta.FBConfigID,
		Version:     cfg.Meta.Version(),
		RedirectURI: cfg.SignupRedirectURI(),
	}, app.pageData, log)

	return app, nil
}

// Router builds the HTTP handler for the whole app.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(httpx.Recoverer(a.Log))
	r.Use(httpx.NewLogger(a.Log, a.Config.TrustProxy).Middleware)
	// CSRF middleware also loads the session, since verifying the token needs it.
	r.Use(a.CSRF.Middleware)

	// Meta's webhook is public by necessity: it is authenticated by an HMAC
	// signature over the body, and the CSRF middleware skips it explicitly.
	a.Webhooks.Routes(r)

	r.Method(http.MethodGet, "/healthz", http.HandlerFunc(a.health))
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(a.staticFS)))

	// Limit credential submissions only. Limiting the GET pages too would let a
	// visitor lock themselves out by reloading the form.
	authRateLimit := a.authLimit.Middleware(httpx.ByIP(a.Config.TrustProxy))
	r.Get("/login", kit.Handler(a.Auth.LoginPage()))
	r.Get("/register", kit.Handler(a.Auth.RegisterPage()))
	r.With(authRateLimit).Post("/login", kit.Handler(a.Auth.Login()))
	r.With(authRateLimit).Post("/register", kit.Handler(a.Auth.Register()))

	r.Group(func(r chi.Router) {
		r.Use(a.Auth.RequireAuth)
		r.Post("/logout", kit.Handler(a.Auth.Logout()))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		})
		r.Get("/dashboard", kit.Handler(a.dashboardPage))

		// Facebook returns the customer to signup.RedirectURI, which is this page,
		// so Index completes a signup that arrives as query parameters.
		r.Get("/connections", kit.Handler(a.Connections.Index))
		// The state endpoint is a POST: it mutates the session, so a GET would
		// let a third-party page prime it from a victim's browser.
		r.Post("/connections/signup-state", kit.Handler(a.Connections.SignupState))
		r.Post("/connections/{id}/disconnect", kit.Handler(a.Connections.Disconnect))
	})

	r.NotFound(kit.Handler(func(k *kit.Kit) error {
		return httpx.NotFound("That page does not exist.")
	}))

	return r
}

func buildStaticFS() (http.FileSystem, error) {
	fsys, err := wasatn.StaticFS()
	if err != nil {
		return nil, fmt.Errorf("app: static assets: %w", err)
	}
	return http.FS(fsys), nil
}

// health reports process and database health. It is unauthenticated so a load
// balancer can reach it.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := a.Pool.Ping(ctx); err != nil {
		a.Log.ErrorContext(r.Context(), "health check failed", "err", err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"unhealthy","database":"down"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok","database":"up"}`)
}

// pageData assembles the shared view model, including any pending flash message.
func (a *App) pageData(k *kit.Kit, title string) web.PageData {
	p := web.PageData{
		Title:     title,
		CSRFToken: a.CSRF.Token(k.Request),
	}
	if user, ok := a.Session.User(k.Request); ok {
		p.User = &web.UserView{Email: user.Email, Plan: string(user.Plan)}
	}
	if kind, message := a.Session.TakeFlash(k.Request); message != "" {
		p.Flash = &web.FlashView{Kind: kind, Message: message}
	}
	return p
}

// dashboardPage renders the landing page.
func (a *App) dashboardPage(k *kit.Kit) error {
	// Campaign counts arrive with that milestone.
	count, err := a.Connections.Count(k.Request.Context(), a.Session.UserID(k.Request))
	if err != nil {
		return httpx.Internal(err)
	}
	return k.Render(web.DashboardPage(a.pageData(k, "Dashboard"), count, 0))
}
