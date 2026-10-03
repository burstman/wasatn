# WasaTN

Multi-tenant WhatsApp automation SaaS. Clients connect their own WhatsApp
Business number through Meta Embedded Signup, manage message templates, and run
scheduled campaigns.

Built with Go, [chi](https://github.com/go-chi/chi), [templ](https://templ.guide),
HTMX, PostgreSQL ([pgx](https://github.com/jackc/pgx) + [sqlc](https://sqlc.dev)),
and [River](https://riverqueue.com) for the job queue.

## Status

Milestones 1 to 3 of the roadmap in [PROMPT.md](PROMPT.md) are implemented: the
app skeleton, PostgreSQL schema, email/password authentication, sessions, CSRF,
rate limiting, the base UI, the worker entrypoint, Meta's webhook endpoint, and
WhatsApp connections through Meta Embedded Signup. Later milestones (templates,
contacts, campaigns, analytics) are not built yet, and their navigation entries
are deliberately disabled.

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
internal/config     environment loading and validation
internal/connections Embedded Signup flow, connections service and handlers
internal/cryptox    AES-256-GCM encryption for access tokens
internal/db         pool, query wrappers, sqlc output
internal/httpx      CSRF, rate limiting, logging, JSON and error responses
internal/jobs       River client and worker wiring
internal/phone      E.164 normalisation, validation and masking
internal/web        templ components and page views
internal/webhooks   Meta webhook signature, challenge, and event handlers
internal/whatsapp   Cloud API client: code exchange, accounts, phone numbers
migrations        paired up/down SQL migrations
static            CSS, vendored htmx and Alpine
```

## Connecting a WhatsApp number

`/connections` runs Meta's **Classic Embedded Signup v4**. The browser asks the
server for a one-shot state value, opens Meta's Facebook Login dialog in a popup
it opens itself, and the dialog hands the authorization code back to
`/connections`, where the server completes the exchange and redirects.

No third-party script is loaded. The dialog is Meta's own
`/dialog/oauth` URL with `config_id`, which is what `FB.login` assembles anyway,
and `window.open` inside the click handler is a user gesture no browser or
popup blocker can refuse. The SDK's `FB.login` was tried first and dropped: it
opens its window from a promise callback, so Firefox refuses it as
un-user-initiated without a word, and it either runs before its own `FB.init`
or not at all.

Setting it up requires four things:

| Where | What |
| --- | --- |
| App settings > Basic | `META_APP_ID` and `META_APP_SECRET` |
| WhatsApp > Embedded Signup | `META_FB_CONFIG_ID`, the configuration id passed to the dialog |
| Facebook Login > Settings | The exact redirect URI below, as a valid OAuth redirect URI |
| App settings > Basic > App domains | The bare host of `PUBLIC_BASE_URL` |

The redirect URI is derived, not configured separately, so it cannot drift from
the deployment:

```sh
PUBLIC_BASE_URL=https://wasatn.onrender.com   # → https://wasatn.onrender.com/connections
```

If any of these is missing the Connect button is not rendered and the startup log
says which variable is empty. Never paste `META_APP_SECRET` into a chat or a
ticket: set it as an environment secret where the app is hosted.

The server then exchanges the code for a short-lived token, trades that for a
**customer-scoped long-lived token** (about 60 days), lists the WhatsApp Business
Accounts it reaches, subscribes the app to each account's webhooks, and stores one
connection per phone number.

A few consequences of that token choice, which differ from `PROMPT.md`:

- Every tenant stores its own token, encrypted with AES-256-GCM. Nothing is
  shared between tenants, and there is no System User token.
- The token expires. WasaTN does not refresh it: `/connections` warns a week
  ahead, shows the expiry date, and the customer reconnects from the same button.
  A connection whose token has run out is not used to send.
- A number already connected to a different WasaTN account is refused with a 409
  rather than silently reassigned, because a WhatsApp number has exactly one owner.
- Embedded Signup v4 lets a business finish without a verified number. Such an
  account is stored as `pending_phone`: visible, but not ready to send from. When
  a number appears later, the placeholder is replaced by a row for that number.
- One WhatsApp Business Account can own several numbers, and all of them are
  connected by the same signup as separate connections.

Sending real customers also requires Meta Business Verification and Advanced
Access for the WhatsApp product. In Development mode only app roles and testers
can complete the dialog.

## Meta webhooks

The app exposes `GET` and `POST /webhooks/whatsapp`, the callback URL you
configure on the WhatsApp product in the Meta app dashboard. Point it at the
public origin of your deployment, for example `https://wasatn.onrender.com/webhooks/whatsapp`.

`GET` answers Meta's subscription challenge by comparing `hub.verify_token` with
`META_VERIFY_TOKEN` and echoing `hub.challenge`. `POST` verifies the
`X-Hub-Signature-256` HMAC over the raw body with `META_APP_SECRET` before
anything is parsed or written, then routes each change by its field:

| Field | Effect |
| --- | --- |
| `messages` | Advances `message_logs` through sent → delivered → read → failed, with Meta's error code and detail on a failure. Inbound customer messages are acknowledged but not yet answered. |
| `account_update` | A removal disconnects the connection and pauses its `draft`, `scheduled` and `sending` campaigns. |
| `message_template_status_update` | Records the review status (`pending`, `approved`, `rejected`, `paused`) and rejection reason. |

Status transitions are monotonic and idempotent: a redelivered webhook, or a
`delivered` that arrives after `read`, changes nothing. Unknown fields, unknown
statuses and unknown `account_update` events are ignored so a Meta change cannot
break processing. A status for a message we never sent is answered 200, because
any other status makes Meta redeliver the same payload for days.

The endpoint is public by necessity and is exempt from CSRF for that reason; it
is authenticated by the HMAC signature instead. Requests with a missing or
malformed signature get 401, a bad signature 403, and an undecodable payload 400.

Subscribe to the `messages`, `message_template_status_update` and
`account_update` fields in the dashboard. The webhook needs
`META_APP_SECRET` and `META_VERIFY_TOKEN` to be set; without them every delivery
is rejected.

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
contacts, templates, campaigns, message logs, and audit logs. `0003` made a
connection's phone number nullable for Embedded Signup v4, and `0004` made the
phone number the identity of a connection, so one WhatsApp Business Account can
own several connected numbers.

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
   string for `DATABASE_URL`, and fill in the Meta fields below if you want
   customers to connect their own numbers.
4. Deploy, then check `curl https://<your-url>/healthz`. It must return
   `{"status":"ok","database":"up"}`.

`PUBLIC_BASE_URL` is filled in from the service's `hostedDomainName`, so cookies
and redirects work without editing it by hand. Do not switch it to the `host`
property: that yields only the short service name (`wasatn`), which would make
the signup redirect URI `https://wasatn/connections`. The app refuses to start in
production when `PUBLIC_BASE_URL` has no dot in its host, precisely so that
mistake cannot ship silently.

| Variable | Required | Notes |
| --- | --- | --- |
| `DATABASE_URL` | yes | Neon connection string. Use the **direct** endpoint, not the `-pooler` one: Render runs a long-lived process and `pgxpool` handles pooling. |
| `SESSION_SECRET` | yes | `openssl rand -hex 32`, minimum 32 bytes. |
| `TOKEN_ENCRYPTION_KEY` | yes | `openssl rand -hex 32`, exactly 32 bytes. Encrypts WhatsApp access tokens at rest. |
| `CRON_SECRET` | yes | `openssl rand -hex 32`. Guards the internal scheduler endpoint. |
| `SUPERKIT_ENV` | blueprint | `production` turns on https and secure cookies. |
| `TRUST_PROXY` | blueprint | Must be `true`: Render terminates TLS and sets `X-Forwarded-Proto` and `X-Forwarded-For`. |
| `PUBLIC_BASE_URL` | blueprint | Derived from `hostedDomainName`; a bare hostname is upgraded to https. Production rejects a host with no dot in it. |
| `PORT` | Render | Injected by the platform and bound when `HTTP_ADDR` is unset. |
| `META_APP_ID`, `META_APP_SECRET`, `META_VERIFY_TOKEN` | no | Without them the app boots and logs a warning, webhooks reject every delivery, and the connections page shows a setup notice. |
| `META_FB_CONFIG_ID` | no | Embedded Signup configuration id. Public, not a secret, but without it the Connect button is never rendered. |

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
