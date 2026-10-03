package auth

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/anthdm/superkit/kit"
	"github.com/burstman/wasatn/internal/httpx"
	"github.com/burstman/wasatn/internal/web"
)

// Handlers serves the authentication pages and form submissions.
type Handlers struct {
	service *Service
	session *Session
	csrf    *httpx.CSRF
	log     *slog.Logger
}

// NewHandlers wires the auth handlers.
func NewHandlers(service *Service, session *Session, csrf *httpx.CSRF, log *slog.Logger) *Handlers {
	return &Handlers{service: service, session: session, csrf: csrf, log: log}
}

// LoginPage renders the sign-in form, redirecting signed-in users away.
func (h *Handlers) LoginPage() kit.HandlerFunc {
	return func(k *kit.Kit) error {
		if _, ok := h.session.User(k.Request); ok {
			return httpx.Redirect(k, http.StatusSeeOther, "/dashboard")
		}
		return k.Render(web.LoginPage(h.pageData(k, "Sign in"), authForm(k, "/login", "Sign in", "Create one", "/register", "current-password")))
	}
}

// Login verifies credentials and starts a session.
func (h *Handlers) Login() kit.HandlerFunc {
	return func(k *kit.Kit) error {
		email := k.Request.PostFormValue("email")
		password := k.Request.PostFormValue("password")

		user, err := h.service.Authenticate(k.Request.Context(), email, password)
		if err != nil {
			if errors.Is(err, ErrInvalidCredentials) {
				return h.renderAuthError(k, authForm(k, "/login", "Sign in", "Create one", "/register", "current-password"), map[string]string{
					"form": "Invalid email or password.",
				})
			}
			return httpx.Internal(err)
		}

		if err := h.session.Login(k.Request, user); err != nil {
			return httpx.Internal(err)
		}
		h.log.InfoContext(k.Request.Context(), "user signed in",
			"user_id", user.ID,
			"request_id", httpx.RequestID(k.Request),
		)
		return httpx.Redirect(k, http.StatusSeeOther, "/dashboard")
	}
}

// RegisterPage renders the sign-up form.
func (h *Handlers) RegisterPage() kit.HandlerFunc {
	return func(k *kit.Kit) error {
		if _, ok := h.session.User(k.Request); ok {
			return httpx.Redirect(k, http.StatusSeeOther, "/dashboard")
		}
		return k.Render(web.RegisterPage(h.pageData(k, "Create account"), authForm(k, "/register", "Create account", "Sign in", "/login", "new-password")))
	}
}

// Register creates an account and signs the user in.
func (h *Handlers) Register() kit.HandlerFunc {
	return func(k *kit.Kit) error {
		email := k.Request.PostFormValue("email")
		password := k.Request.PostFormValue("password")

		user, err := h.service.Register(k.Request.Context(), email, password)
		if err != nil {
			var fieldErrs FieldErrors
			switch {
			case errors.As(err, &fieldErrs):
				return h.renderAuthError(k, authForm(k, "/register", "Create account", "Sign in", "/login", "new-password"), fieldErrs)
			case errors.Is(err, ErrEmailTaken):
				return h.renderAuthError(k, authForm(k, "/register", "Create account", "Sign in", "/login", "new-password"), map[string]string{
					"email": "An account with that email already exists.",
				})
			default:
				return httpx.Internal(err)
			}
		}

		if err := h.session.Login(k.Request, user); err != nil {
			return httpx.Internal(err)
		}
		h.log.InfoContext(k.Request.Context(), "user registered",
			"user_id", user.ID,
			"request_id", httpx.RequestID(k.Request),
		)
		return httpx.Redirect(k, http.StatusSeeOther, "/dashboard")
	}
}

// Logout destroys the session.
func (h *Handlers) Logout() kit.HandlerFunc {
	return func(k *kit.Kit) error {
		if err := h.session.Logout(k.Request); err != nil {
			return httpx.Internal(err)
		}
		return httpx.Redirect(k, http.StatusSeeOther, "/login")
	}
}

// renderAuthError re-renders the form with errors. HTMX requests get only the
// form fragment so the surrounding page is preserved.
func (h *Handlers) renderAuthError(k *kit.Kit, form web.AuthForm, errs map[string]string) error {
	form.Errors = errs
	form.Email = k.Request.PostFormValue("email")

	k.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	k.Response.WriteHeader(http.StatusUnprocessableEntity)

	if httpx.IsHTMX(k) {
		return web.AuthFormFields(h.pageData(k, ""), form).Render(k.Request.Context(), k.Response)
	}

	var page templ.Component
	if form.Action == "/register" {
		page = web.RegisterPage(h.pageData(k, "Create account"), form)
	} else {
		page = web.LoginPage(h.pageData(k, "Sign in"), form)
	}
	return page.Render(k.Request.Context(), k.Response)
}

// RequireAuth rejects anonymous requests.
func (h *Handlers) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.session.User(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// pageData assembles the shared view model for a request.
func (h *Handlers) pageData(k *kit.Kit, title string) web.PageData {
	p := web.PageData{
		Title:     title,
		CSRFToken: h.csrf.Token(k.Request),
	}
	if user, ok := h.session.User(k.Request); ok {
		p.User = &web.UserView{Email: user.Email, Plan: string(user.Plan)}
	}
	if kind, message := h.session.TakeFlash(k.Request); message != "" {
		p.Flash = &web.FlashView{Kind: kind, Message: message}
	}
	return p
}

func authForm(k *kit.Kit, action, submit, alternateText, alternateHref, passwordAutocomplete string) web.AuthForm {
	return web.AuthForm{
		Email:                k.Request.PostFormValue("email"),
		Action:               action,
		Submit:               submit,
		Alternate:            alternateText,
		AlternateHref:        alternateHref,
		PasswordAutocomplete: passwordAutocomplete,
		Errors:               map[string]string{},
	}
}
