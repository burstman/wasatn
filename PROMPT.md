# Build: WhatsApp Automation SaaS (Go + HTMX)

You are a senior Go engineer. Build a production-ready multi-tenant SaaS that lets clients connect their own WhatsApp Business number via Meta Embedded Signup, create and manage WhatsApp message templates (UTILITY category focus), validate them before submission, and run automated/scheduled campaigns.

## Tech Stack
- Go 1.26+ with chi router (net/http stdlib based) — 1.26 is the floor because River requires it
- PostgreSQL with pgx + sqlc (schema in migrations/) — strictly typed, no ORM
- Redis + River (or Asynq) for the job queue (send worker + scheduled/recurring jobs)
- a-h/templ for all HTML generation (run `templ generate` in watch mode)
- HTMX for all dynamic interactions (hx-get/hx-post/hx-target, hx-swap; polling for progress)
- Alpine.js for client-side-only behavior (dropdowns, modals, form validation UX)
- TailwindCSS + daisyUI via the Tailwind CLI
- Auth: server-side sessions (gorilla/sessions or scs) with email+password, bcrypt passwords

## Project Layout
cmd/server/main.go | internal/{auth,connections,templates,campaigns,webhooks,whatsapp,jobs,contacts,analytics} | migrations/ | templ/ | static/ | webhook payloads samples in testdata/

## Core Features (MVP)

### 1. WhatsApp Connection (Embedded Signup)
- "Connect WhatsApp" button launching Meta Embedded Signup (Facebook Login JS SDK + embedded signup flow)
- Flow: embedded signup returns auth code -> exchange for token -> fetch WABA + phone numbers -> store connection
- Store per connection: messaging_account_id (= WABA ID), phone_number_id, display name, encrypted access token, token expiry, status (active/disconnected), phone_number
- Exchange short-lived token for long-lived System User token (60 days); implement refresh logic before expiry
- A user can connect multiple WhatsApp numbers; list/manage them in a connections page
- Handle account_update webhook: when client removes access, mark connection disconnected and pause all its campaigns

### 2. Template Manager (UTILITY focus)
- Pages (templ components + HTMX):
  - Template list (synced from Meta, status badges: PENDING/APPROVED/REJECTED/PAUSED)
  - Create/edit form with live Alpine.js preview rendering the template as the client would see it
- Fields: name (snake_case), category (UTILITY default; MARKETING, AUTHENTICATION), language, header TEXT, body with {{1}}, {{2}} placeholders, footer, buttons (QUICK_REPLY, URL with dynamic suffix)
- SERVER-SIDE VALIDATION (Go package internal/templates/validate.go, fully unit-tested) + client-side Alpine hints:
  - Placeholders sequential from {{1}}, no gaps, no duplicates; max 5 body vars
  - Body <= 1024 chars, header <= 60, footer <= 60
  - Category rules: UTILITY must indicate order/account update, alert, or feedback
  - Warn on URLs/phone/email in body (requires WABA allowlist)
  - Name: lowercase snake_case regex, no spaces
- Submit via POST /{messaging_account_id}/message_templates (Graph API v21+)
- Webhook message_template_status_update updates statuses; show rejection reason; allow edit+resubmit
- Placeholder extractor: parse {{n}} from body on save; store vars so campaign forms render per-variable input fields

### 3. Automation Engine (Campaigns)
- Campaign wizard (multi-step, HTMX + Alpine state):
  1. Pick approved template
  2. Upload recipients CSV (E.164 phones, columns map to variables) or pick a saved contact list
  3. Column -> variable mapping UI
  4. Schedule: now / at datetime / recurring (daily/weekly/cron expr)
- Enqueue one River/Asynq job per recipient (chunked), with per-phone-number concurrency limit (default 50 msg/sec, configurable per connection tier)
- Worker sends POST https://graph.facebook.com/v21.0/{phone_number_id}/messages?messaging_account_id={id}
- Payload: {messaging_product:"whatsapp", to:"<e164>", type:"template", template:{name, language:{code}, components:[{type:"body", parameters:[...]}]}}
- Retry policy: exponential backoff on 130429 + 80007 (rate limit), max N attempts; permanent fail on 131026 (not on WA), 131049 (template paused), 132000 (number banned) -> fail fast + alert user
- Campaign states: draft/scheduled/sending/paused/completed/failed
- Recurring campaigns: River periodic job re-enqueues; skip when connection disconnected
- Opt-in gate: refuse to send to contacts without opt_in=true + opt_in_at recorded; log every send decision
- CSV parsing: stream with encoding/csv, validate + normalize via libphonenumber (or manual E.164 check), report bad rows to user

### 4. Webhooks Endpoint
- POST /webhooks/whatsapp: verify X-Hub-Signature-256 (HMAC with app secret), then route by field
- Handle: messages (statuses sent/delivered/read/failed + errors), message_template_status_update, account_update
- GET /webhooks/whatsapp: hub.challenge verification with META_VERIFY_TOKEN
- Update MessageLog rows for analytics; idempotent processing (dedupe by wamid + status)
- Testdata: include sample Meta webhook JSON payloads + tests

### 5. Dashboard & Analytics
- Campaign detail: sent/delivered/read/failed counts + rates, failure-reason breakdown, live HTMX polling (2-3s) while sending
- Connection page: quality rating, messaging limit, monthly send count (for your SaaS metering), token status
- Contacts CRUD + CSV import/export + opt-in toggle

## Data Model (migrations/)
users(id, email, password_hash, plan, created_at)
connections(id, user_id, messaging_account_id, waac_id TEXT NULL, phone_number_id, phone_number, display_name, access_token_encrypted, token_expires_at, quality_rating, messaging_limit, status)
contacts(id, user_id, phone, name, opt_in, opt_in_at, variables JSONB)
templates(id, connection_id, meta_template_id TEXT, name, category, language, components JSONB, variables TEXT[], status, rejection_reason TEXT)
campaigns(id, user_id, connection_id, template_id, name, schedule JSONB, status, stats counters, created_at)
message_logs(id, campaign_id, contact_id, wamid, status, error_code TEXT, error_message TEXT, timestamps)
audit_logs(id, user_id, action, entity, entity_id, meta JSONB, created_at)

## WhatsApp API Integration Notes (IMPORTANT)
- Graph API v21 (or latest); all calls via net/http with context timeouts + retry on 5xx/timeouts
- Template endpoints keyed by Messaging Account ID (= current WABA ID): POST/GET/DELETE /{id}/message_templates
- ALWAYS include messaging_account_id query param on Messages API calls
- 401 handling: attempt token refresh once; else mark token_expired + alert
- New account model readiness: store waac_id (nullable) even though unavailable until Meta Phase 2

## Security & Compliance
- AES-256-GCM encrypt access tokens (key from env); decrypt only in-memory at send time
- Env only for secrets: never commit; use .env.example
- E.164 validation everywhere; mask phone numbers in logs (last 4 digits visible)
- CSRF protection for HTMX forms (double-submit cookie or header check)
- Rate-limit auth endpoints + campaign creation (per-user)
- audit_logs for every template/campaign/send action

## Environment Variables (.env.example)
DATABASE_URL, REDIS_URL, SESSION_SECRET, TOKEN_ENCRYPTION_KEY, META_APP_ID, META_APP_SECRET, META_VERIFY_TOKEN, PUBLIC_BASE_URL, CRON_SECRET

## Deliverables (milestone by milestone, commit after each)
1. Go project scaffold: chi router, migrations, sqlc setup, session auth, templ+Tailwind+HTMX+Alpine base layout, River/Asynq worker skeleton
2. Webhook endpoint (signature verification + handlers + tests with testdata payloads)
3. Embedded Signup connection flow + token exchange/refresh + connections UI
4. Template manager: CRUD UI + validation package (unit tested) + Meta sync + status webhooks
5. Campaign engine: CSV import, variable mapping, send worker with throttling/retries, send-now
6. Scheduling/recurring + analytics dashboard + contacts + opt-in management
7. Polish: error handling, empty states, HTMX loading states, README (setup, Meta app config, templ generate + tailwind build steps)

## Rules for the agent
- Run `go test ./...` and `go vet ./...` must pass before committing each milestone
- sqlc + templ code is generated: always regenerate after schema/templ changes (Makefile targets)
- Write table-driven tests for validation, webhook verification, CSV parsing, retry logic
- Strong typing: no interface{} in core logic; define domain types in internal/<pkg>/types.go
- Never log tokens or full phone numbers in plaintext
- Mark uncertain Meta API fields with TODO(META-DOC) and implement documented Cloud API behavior
- Ask me before making unspecified product decisions
