# WasaTN

Multi-tenant WhatsApp automation SaaS. Clients connect their own WhatsApp
Business number through Meta Embedded Signup, manage message templates, and run
scheduled campaigns.

Built with Go, [chi](https://github.com/go-chi/chi), [templ](https://templ.guide),
HTMX, PostgreSQL ([pgx](https://github.com/jackc/pgx) + [sqlc](https://sqlc.dev)),
and [River](https://riverqueue.com) for the job queue.

## Status

Milestone 1 of the roadmap in [PROMPT.md](PROMPT.md) is implemented: the app
skeleton, PostgreSQL schema, email/password authentication, sessions, CSRF,
rate limiting, the base UI, and the worker entrypoint. Later milestones
(connections, templates, contacts, campaigns, analytics) are not built yet, and
their navigation entries are deliberately disabled.

## Requirements

- Go 1.26 or newer (required by the River job queue)
- PostgreSQL 13 or newer, local or hosted (the intended target is Neon)
- Node.js 20+ only for regenerating CSS and vendored frontend assets

## Getting started

```sh
cp .env.example .env      # then fill in the values
make migrate              # apply the app schema and River's migrations
make run                  # start the web server on :8080
```

Run the job worker in a second terminal:

```sh
make worker
```

For hot reload, install [air](https://github.com/air-verse/air) and run
`make dev`, which runs the server and worker together.

## Configuration

All configuration is read from the environment; see [.env.example](.env.example)
for the full list. The values you must supply before the app will boot:

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string |
| `SESSION_SECRET` | At least 32 bytes (`openssl rand -base64 48`) |
| `TOKEN_ENCRYPTION_KEY` | Exactly 32 bytes as base64 or 64-char hex (`openssl rand -base64 32`); encrypts stored WhatsApp access tokens |

`SUPERKIT_ENV=production` additionally enforces HTTPS for `PUBLIC_BASE_URL` and
requires the Meta credentials plus `CRON_SECRET`. Set `TRUST_PROXY=true` only
when the app runs behind a proxy you control; otherwise rate limiting trusts the
`X-Forwarded-For` header and clients can spoof their address.

Secrets are never written to logs. `TOKEN_ENCRYPTION_KEY` is required in every
environment because tokens cannot be decrypted without it.

## Layout

```
cmd/server        web server entrypoint
cmd/worker        River worker entrypoint
cmd/migrate       migration runner (app SQL + River)
internal/auth     registration, login, bcrypt, sessions
internal/config   environment loading and validation
internal/cryptox  AES-256-GCM encryption for access tokens
internal/db       pool, query wrappers, sqlc output
internal/httpx    CSRF, rate limiting, logging, JSON and error responses
internal/jobs     River client and worker wiring
internal/phone    E.164 validation and masking
internal/web      templ components and page views
migrations        paired up/down SQL migrations
static            CSS, vendored htmx and Alpine
```

## Database

Migrations use paired files (`NNNN_name.up.sql` and `NNNN_name.down.sql`) and are
applied with [golang-migrate](https://github.com/golang-migrate/migrate):

```sh
make migrate-new name=add_campaign_budget   # scaffold a migration
make migrate                                 # apply everything
make migrate-down                            # roll back one step
make migrate-version                         # show applied versions
```

`migrations/0001_init.up.sql` holds the whole MVP schema: users, connections,
contacts, templates, campaigns, message logs, and audit logs.

## Development

```sh
make generate    # sqlc, templ, CSS, and vendored assets
make check       # gofmt, go vet, and the test suite
make test-race   # tests with the race detector
make cover       # coverage summary
```

Generated code (`internal/db/sqlc`, `*_templ.go`, `static/css/app.css`,
`static/vendor`) is committed so a fresh clone builds without extra toolchains.
Regenerate and commit it whenever the source templates, queries, or migrations
change.

## Tests

Unit tests cover the token encryption, phone number rules, credential
validation and authentication service, session and CSRF middleware, rate
limiter, request middleware, configuration loading, and job registration.
These need no database and run with `make test`.

The River integration test needs a real Postgres database. It inserts a job and
asserts a worker picks it up and completes it, so point it at a throwaway
database:

```sh
TEST_DATABASE_URL='postgres://...' go test ./internal/jobs/ -run Integration
```

It is skipped when `TEST_DATABASE_URL` is unset, so `make check` stays safe to
run anywhere.

## Deploying to Render

`render.yaml` is a [Render blueprint](https://render.com/docs/blueprint-spec): a
single web service that builds one static binary from `./cmd/server`. Render
needs no Node toolchain, because the generated templ and CSS output is committed
and static assets are embedded with `go:embed`.

Steps:

1. Push the repo to GitHub.
2. In Render choose **New > Blueprint**, select the repo, and apply.
3. Render shows the fields from `render.yaml`. Give it the Neon connection
   string for `DATABASE_URL`, and leave the Meta fields empty until milestone 2.
4. Deploy, then check `curl https://<your-url>/healthz`. It must return
   `{"status":"ok","database":"up"}`.

`PUBLIC_BASE_URL` is filled in from the service host, so cookies and redirects
work without editing it by hand.

| Variable | Required | Notes |
| --- | --- | --- |
| `DATABASE_URL` | yes | Neon connection string. Use the **direct** endpoint, not the `-pooler` one: Render runs a long-lived process and `pgxpool` handles pooling. |
| `SESSION_SECRET` | yes | `openssl rand -hex 32`, minimum 32 bytes. |
| `TOKEN_ENCRYPTION_KEY` | yes | `openssl rand -hex 32`, exactly 32 bytes. Encrypts WhatsApp access tokens at rest. |
| `CRON_SECRET` | yes | `openssl rand -hex 32`. Guards the internal scheduler endpoint. |
| `SUPERKIT_ENV` | blueprint | `production` turns on https and secure cookies. |
| `TRUST_PROXY` | blueprint | Must be `true`: Render terminates TLS and sets `X-Forwarded-Proto` and `X-Forwarded-For`. |
| `PUBLIC_BASE_URL` | blueprint | Derived from the service host; a bare hostname is upgraded to https. |
| `PORT` | Render | Injected by the platform and bound when `HTTP_ADDR` is unset. |
| `META_APP_ID`, `META_APP_SECRET`, `META_VERIFY_TOKEN` | no | Empty until milestone 2. The app boots and logs a warning, and the connections page shows a setup notice. |

Migrations are **not** run by Render, because the free plan has no pre-deploy
command. Run them from your machine whenever the schema changes:

```sh
make migrate          # applies migrations/*.up.sql plus River's migrations
```

Things to know about the free plan:

- The service spins down after 15 minutes of inactivity, so the first request
  after a pause is slow. Sessions live in PostgreSQL, so logins survive.
- The login rate limiter is in-process memory, so its counters reset on every
  restart and are not shared across instances. Fine for testing; revisit it
  before real users arrive.
- Render Background Workers need a paid plan, and the worker has no job kinds to
  run until milestone 5, so `render.yaml` ships the web service alone.
