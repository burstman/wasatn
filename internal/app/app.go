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
	"github.com/burstman/wasatn/internal/db"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/httpx"
	"github.com/burstman/wasatn/internal/jobs"
	"github.com/burstman/wasatn/internal/web"
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
	Config  *config.Config
	Log     *slog.Logger
	Pool    *pgxpool.Pool
	Queries *sqlc.Queries
	Session *auth.Session
	CSRF    *httpx.CSRF
	Auth    *auth.Handlers
	Jobs    *jobs.Client

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
	service := auth.NewService(db.NewQueries(pool), auth.BcryptCost, log)

	jobClient, err := jobs.NewClient(pool, &cfg.River, nil, log.With("component", "river"))
	if err != nil {
		return nil, err
	}

	return &App{
		Config:    cfg,
		Log:       log,
		Pool:      pool,
		Queries:   db.NewQueries(pool),
		Session:   session,
		CSRF:      csrf,
		Auth:      auth.NewHandlers(service, session, csrf, log.With("component", "auth")),
		Jobs:      jobClient,
		staticFS:  staticFS,
		authLimit: httpx.NewLimiter(AuthRateLimitAttempts, AuthRateLimitWindow),
	}, nil
}

// Router builds the HTTP handler for the whole app.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(httpx.Recoverer(a.Log))
	r.Use(httpx.NewLogger(a.Log, a.Config.TrustProxy).Middleware)
	// CSRF middleware also loads the session, since verifying the token needs it.
	r.Use(a.CSRF.Middleware)

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
		r.Get("/connections", kit.Handler(a.connectionsPage))
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
	// Connection and campaign counts arrive with those milestones.
	return k.Render(web.DashboardPage(a.pageData(k, "Dashboard"), 0, 0))
}

// connectionsPage renders the connections list.
func (a *App) connectionsPage(k *kit.Kit) error {
	return k.Render(web.ConnectionsPage(a.pageData(k, "Connections"), a.Config.Meta.MetaConfigured()))
}
