-- Project-level schema: all per-project tables (no project_id columns).
-- Each project gets its own SQLite database at data/projects/{id}/project.db.

-- Artifact store for every file-like thing the builder produces. kind is a
-- free string the LLM picks; the platform does not branch on it.
--   page | layout | component | file  (path = served path, or name for layout/component)
-- storage says where the bytes live: 'inline' = body, 'disk' = disk_path.
-- metadata is free-form per-kind JSON. scope 'global' auto-injects on every
-- page; 'page' must be linked in of_page_assets.
CREATE TABLE IF NOT EXISTS of_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    path TEXT NOT NULL,
    storage TEXT NOT NULL DEFAULT 'inline',
    body TEXT DEFAULT '',
    disk_path TEXT DEFAULT '',
    content_type TEXT DEFAULT '',
    metadata TEXT DEFAULT '{}',
    scope TEXT DEFAULT 'global',
    size INTEGER DEFAULT 0,
    status TEXT DEFAULT 'published',
    is_deleted INTEGER DEFAULT 0,
    deleted_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(kind, path)
);

CREATE INDEX IF NOT EXISTS idx_files_kind ON of_files(kind);
CREATE INDEX IF NOT EXISTS idx_files_kind_path ON of_files(kind, path);
CREATE INDEX IF NOT EXISTS idx_files_path ON of_files(path);

-- Per-write attribution log for every artifact kind: one row per save/delete,
-- recording which actor (changed_by) produced each version. Backs the monotonic
-- `version` counter and the build journal's per-task file set. body_hash
-- references of_snapshot_blobs.hash for inline text artifacts (empty for disk
-- artifacts and delete markers); snapshots are capped per artifact so history
-- stays bounded.
CREATE TABLE IF NOT EXISTS of_file_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER,
    kind TEXT NOT NULL,
    path TEXT NOT NULL,
    version_number INTEGER NOT NULL,
    changed_by TEXT NOT NULL DEFAULT 'brain',
    body_hash TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Content-addressed body snapshots backing of_file_history.body_hash: one row
-- per unique body, keyed by its byte hash, so identical versions share a row.
-- refcount tracks referencing history rows; the blob is deleted at refcount 0.
-- Distinct from of_blobs, the general-purpose on-disk blob store.
CREATE TABLE IF NOT EXISTS of_snapshot_blobs (
    hash TEXT PRIMARY KEY,
    body BLOB NOT NULL,
    size INTEGER NOT NULL,
    refcount INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_file_history_lookup
    ON of_file_history(kind, path, version_number DESC);
CREATE INDEX IF NOT EXISTS idx_file_history_date
    ON of_file_history(created_at DESC);

-- Page-scope artifact links: when serving page_path, include artifact file_path
-- in its injected assets. Relational (not a metadata JSON array) so renaming an
-- artifact leaves no dangling reference.
CREATE TABLE IF NOT EXISTS of_page_assets (
    page_path TEXT NOT NULL,
    file_path TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (page_path, file_path)
);

CREATE INDEX IF NOT EXISTS idx_page_assets_page ON of_page_assets(page_path);
CREATE INDEX IF NOT EXISTS idx_page_assets_file ON of_page_assets(file_path);

-- Key-value memory store
CREATE TABLE IF NOT EXISTS of_memory (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT NOT NULL UNIQUE,
    value TEXT NOT NULL,
    category TEXT DEFAULT 'general',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Chat messages (brain + user sessions)
CREATE TABLE IF NOT EXISTS of_chat_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    tool_calls TEXT,
    tool_call_id TEXT,
    metadata TEXT DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_chat_session ON of_chat_messages(session_id, created_at);

-- Questions and answers (human-in-the-loop)
-- status values: 'pending' | 'answered' | 'deferred' | 'superseded'
--   pending    : awaiting owner input, brain stays paused
--   answered   : owner supplied a value
--   deferred   : owner opted to provide later; treated as resolved for brain resume
--   superseded : replaced by a fresh PLAN cycle
CREATE TABLE IF NOT EXISTS of_questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    question TEXT NOT NULL,
    context TEXT,
    options TEXT,
    urgency TEXT DEFAULT 'normal',
    status TEXT DEFAULT 'pending',
    type TEXT DEFAULT 'text',
    secret_name TEXT,
    fields TEXT,
    -- Worker mode when asked: 'build' (PLAN/BUILD) or 'live'. Answers route by
    -- this origin, not answer-time state, so a build question answered after
    -- go-live isn't misread as a live issue. Defaults to 'build'.
    origin TEXT NOT NULL DEFAULT 'build',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_questions_status_date ON of_questions(status, created_at DESC);

CREATE TABLE IF NOT EXISTS of_answers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    question_id INTEGER NOT NULL REFERENCES of_questions(id) ON DELETE CASCADE,
    answer TEXT NOT NULL,
    answered_by INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_answers_question_date ON of_answers(question_id, created_at DESC);

-- Dynamic tables registry (LLM-created schemas)
CREATE TABLE IF NOT EXISTS of_dynamic_tables (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    table_name TEXT NOT NULL UNIQUE,
    schema_def TEXT NOT NULL,
    secure_columns TEXT DEFAULT '{}',
    validation TEXT DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- API endpoints
CREATE TABLE IF NOT EXISTS of_api_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    methods TEXT DEFAULT '["GET","POST"]',
    public_columns TEXT,
    -- writable_columns: JSON array of columns a client write may set. NULL means
    -- any column; owner and tenant columns are stamped from the session either way.
    writable_columns TEXT,
    requires_auth BOOLEAN DEFAULT 0,
    public_read BOOLEAN DEFAULT 0,
    -- public_write: with requires_auth=1, allow writes (POST/PUT/DELETE) without
    -- a token while GET stays auth-gated (the "public submission" shape: anyone
    -- POSTs, only an authed admin reads). Mutually exclusive with
    -- owner_column/tenant_column (an anonymous writer cannot be row-scoped).
    public_write BOOLEAN DEFAULT 0,
    required_role TEXT DEFAULT NULL,
    rate_limit INTEGER DEFAULT 60,
    cors_origins TEXT DEFAULT '',
    cors_methods TEXT DEFAULT '',
    cors_headers TEXT DEFAULT '',
    owner_column TEXT DEFAULT NULL,
    -- owner_scope: how owner_column gates access when it is set.
    --   ''         the owner reads AND writes only their own rows (private per user).
    --   'authored' owner_column marks the row's author: it is stamped on insert and
    --              scopes writes (PUT/DELETE act only on the caller's own rows), but
    --              reads are shared across authenticated users. The collaborative shape
    --              (a chat's messages, a shared board's cards) where members co-read one
    --              set of rows and each person edits only what they wrote. Composes with
    --              tenant_column, which still scopes reads to the caller's tenant.
    owner_scope TEXT NOT NULL DEFAULT '',
    -- tenant_column: column on table_name that holds the row's tenant_id.
    -- When set, every CRUD operation auto-applies AND <tenant_column> = <claims.tenant_id>,
    -- stacking with owner_column for row-level multi-tenant isolation.
    tenant_column TEXT DEFAULT NULL,
    -- lookup_column: TEXT column resolving GET/PUT/DELETE /api/<path>/<value>.
    -- NULL = auto-detect (uuid, then slug, then the sole non-id UNIQUE TEXT
    -- column). Set explicitly when the identifier has a domain name (rooms.code)
    -- and the table has more than one UNIQUE TEXT column.
    lookup_column TEXT DEFAULT NULL,
    cache_ttl INTEGER DEFAULT 0,
    -- read_only: when 1 the endpoint serves GET only; the runtime rejects
    -- POST/PUT/DELETE. Set when the backing object is a SQL VIEW (no CRUD
    -- metadata, cannot be written).
    read_only BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Custom server-side function endpoints (manage_function). Each row mounts a
-- sandboxed, model-authored JS handler(req, ctx) at /api/<path>, run in a goja
-- sandbox (internal/funcsandbox): no require/fs/net/process, I/O only via ctx
-- (read-only queries, declared writes, SSRF-guarded fetch, named secrets).
-- Validated at save time with the runtime's Compile, so valid-on-save ==
-- valid-at-runtime.
CREATE TABLE IF NOT EXISTS of_function_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    code TEXT NOT NULL DEFAULT '',
    -- Comma-separated HTTP methods this endpoint answers (e.g. "GET,POST").
    -- Empty = POST only, the default for an action endpoint.
    methods TEXT NOT NULL DEFAULT '',
    requires_auth BOOLEAN DEFAULT 0,
    cors_origins TEXT DEFAULT '',
    -- Inbound-request rate limit, requests/minute per client IP.
    rate_limit INTEGER NOT NULL DEFAULT 60,
    -- Per-invocation wall-clock cap in milliseconds (0 = the funcsandbox default).
    timeout_ms INTEGER NOT NULL DEFAULT 0,
    -- JSON array of secret names the handler may read via ctx.secret. Empty = the
    -- handler may read no secrets. An allowlist, so a handler can never reach a
    -- stored secret it was not explicitly granted.
    secrets TEXT NOT NULL DEFAULT '',
    -- One line on what the function does and the body it expects. Shown to a hosted
    -- agent that may call the function; empty falls back to a generic line.
    description TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Auth endpoints
CREATE TABLE IF NOT EXISTS of_auth_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    table_name TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    username_column TEXT NOT NULL DEFAULT 'email',
    password_column TEXT NOT NULL DEFAULT 'password',
    public_columns TEXT DEFAULT '[]',
    -- signup_columns: JSON array of extra columns /register may set from the
    -- request body (beyond username and password). Empty falls back to dropping
    -- privilege-like column names.
    signup_columns TEXT DEFAULT '[]',
    jwt_expiry_hours INTEGER DEFAULT 24,
    default_role TEXT NOT NULL DEFAULT 'user',
    role_column TEXT NOT NULL DEFAULT 'role',
    -- rate_limit: requests/min per IP for register/login on this auth endpoint.
    rate_limit INTEGER NOT NULL DEFAULT 60,
    -- tenant_signup_mode controls what happens on register:
    --   none                  - tenancy not enabled on this auth endpoint
    --   invite_only           - require ?invite=<token>, consume of_tenant_invites row
    --   signup_creates_tenant - create a new of_tenants row, user becomes owner
    --   signup_joins_default  - add user to the default tenant
    tenant_signup_mode TEXT NOT NULL DEFAULT 'none',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- OAuth providers (social login)
CREATE TABLE IF NOT EXISTS of_oauth_providers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    client_id TEXT NOT NULL,
    client_secret_name TEXT NOT NULL,
    authorize_url TEXT NOT NULL,
    token_url TEXT NOT NULL,
    userinfo_url TEXT NOT NULL,
    scopes TEXT NOT NULL DEFAULT 'openid email profile',
    username_field TEXT NOT NULL DEFAULT 'email',
    auth_endpoint_path TEXT NOT NULL,
    is_enabled BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Revoked sessions of the project's OWN users (the people who sign in to the app
-- that was built, not the admin). Their tokens are stateless, so a password reset,
-- a deleted or banned user or a changed role is recorded here: tokens issued before
-- revoked_before (unix milliseconds) are refused. Only revoked users have a row.
-- See internal/tokenrevoke.
CREATE TABLE IF NOT EXISTS of_token_revocations (
    user_id INTEGER PRIMARY KEY,
    revoked_before INTEGER NOT NULL
);

-- Webhooks
CREATE TABLE IF NOT EXISTS of_webhooks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    -- The HMAC signing secret, AES-GCM encrypted like of_secrets.value_encrypted
    -- (security.Encryptor.SealOptional). Empty means no secret is set, which an
    -- incoming webhook rejects until one is.
    secret_encrypted TEXT NOT NULL DEFAULT '',
    url TEXT,
    direction TEXT DEFAULT 'incoming',
    is_enabled BOOLEAN DEFAULT 1,
    last_triggered DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_webhook_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_id INTEGER NOT NULL REFERENCES of_webhooks(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(webhook_id, event_type)
);

CREATE TABLE IF NOT EXISTS of_webhook_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_id INTEGER NOT NULL REFERENCES of_webhooks(id) ON DELETE CASCADE,
    direction TEXT NOT NULL,
    event_type TEXT,
    payload TEXT,
    status_code INTEGER,
    response TEXT,
    success BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_webhook_logs_date ON of_webhook_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_of_webhook_logs_webhook_date ON of_webhook_logs(webhook_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_subs_webhook ON of_webhook_subscriptions(webhook_id);

-- Analytics (page views)
CREATE TABLE IF NOT EXISTS of_analytics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    page_path TEXT NOT NULL,
    visitor_hash TEXT,
    referrer TEXT,
    user_agent TEXT,
    country TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_analytics_date ON of_analytics(created_at);
CREATE INDEX IF NOT EXISTS idx_analytics_page_path ON of_analytics(page_path, created_at DESC);

-- Secrets (encrypted key-value)
CREATE TABLE IF NOT EXISTS of_secrets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    value_encrypted TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Scheduled tasks
CREATE TABLE IF NOT EXISTS of_scheduled_tasks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    description TEXT,
    cron_expression TEXT,
    interval_seconds INTEGER,
    prompt TEXT,
    native_action TEXT DEFAULT '',
    is_enabled BOOLEAN DEFAULT 1,
    last_run DATETIME,
    next_run DATETIME,
    run_count INTEGER DEFAULT 0,
    error_count INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    -- idle_run_count: consecutive no-op runs for prompt-backed scheduled tasks,
    -- persisted so the idle backoff survives a process restart.
    idle_run_count INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS of_task_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id INTEGER NOT NULL REFERENCES of_scheduled_tasks(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    result TEXT,
    error_message TEXT,
    started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_of_task_runs_task ON of_task_runs(task_id, started_at DESC);

-- Activity log
CREATE TABLE IF NOT EXISTS of_activity_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,
    summary TEXT,
    details TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_activity_log_date ON of_activity_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_of_activity_log_type ON of_activity_log(event_type, created_at DESC);

-- Bounded ring of failures raised by sandboxed project code at request time.
-- The public response is deliberately generic, so the thrown cause survives
-- only here, where the endpoint probe and diagnostics tool can report it.
--   source  which runtime raised it (currently "function").
--   path    the endpoint path, so a probe reads only its own errors.
CREATE TABLE IF NOT EXISTS of_runtime_errors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_runtime_errors_target
    ON of_runtime_errors(source, path, id DESC);

-- Service providers (external API integrations)
CREATE TABLE IF NOT EXISTS of_service_providers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    base_url TEXT NOT NULL,
    auth_type TEXT DEFAULT 'bearer',
    auth_header TEXT DEFAULT 'Authorization',
    auth_prefix TEXT DEFAULT 'Bearer',
    secret_name TEXT,
    description TEXT DEFAULT '',
    api_docs TEXT DEFAULT '',
    is_enabled BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    -- allow_private_host: scoped opt-in letting this provider target a private/LAN
    -- host (e.g. a Home Assistant hub at 192.168.x). Default 0 keeps the SSRF block
    -- on; honored only for requests whose host matches the provider's base_url.
    allow_private_host INTEGER NOT NULL DEFAULT 0
);

-- Per-project opt-in to the global connector registry (global_connectors in the
-- system DB): a global connector is usable only when its name is enabled here.
-- The engine resolves project-local definitions first, so a local name shadows a
-- global one.
CREATE TABLE IF NOT EXISTS of_enabled_connectors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    global_name TEXT NOT NULL UNIQUE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Upload endpoints (file upload routes)
CREATE TABLE IF NOT EXISTS of_upload_endpoints (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    allowed_types TEXT,
    max_size_mb INTEGER DEFAULT 5,
    requires_auth BOOLEAN DEFAULT 0,
    -- rate_limit: requests/min per IP for this upload endpoint, enforced by the
    -- public upload handler (0 disables the cap).
    rate_limit INTEGER NOT NULL DEFAULT 60,
    table_name TEXT,
    -- owner_column: column on table_name auto-set from claims.UserID on insert.
    owner_column TEXT DEFAULT '',
    -- tenant_column: column on table_name auto-set from claims.TenantID on insert.
    -- Also prefixes the stored file path with /t<tenant_id>/ so guessable URLs
    -- do not cross tenants.
    tenant_column TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_upload_endpoints_table ON of_upload_endpoints(table_name);

-- SSE stream endpoints
CREATE TABLE IF NOT EXISTS of_stream_endpoints (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    event_types TEXT,
    -- table_name scopes the stream to one source: when set, only row events whose
    -- payload "table" matches are pushed. Empty = every data event of the
    -- configured types across the project.
    table_name TEXT,
    requires_auth BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- LLM chat/completion endpoints
CREATE TABLE IF NOT EXISTS of_llm_endpoints (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    system_prompt TEXT NOT NULL DEFAULT '',
    model_id TEXT DEFAULT '',
    max_tokens INTEGER DEFAULT 4096,
    temperature REAL DEFAULT 0.7,
    max_history INTEGER DEFAULT 20,
    rate_limit INTEGER DEFAULT 10,
    -- daily_token_budget caps per-endpoint per-UTC-day spend through the LLM proxy
    -- (0 = unlimited). daily_tokens_used / day_window_start are the proxy's
    -- counter, rolled forward on the first hit each UTC day (previous total
    -- archived into of_llm_usage_daily).
    daily_token_budget INTEGER NOT NULL DEFAULT 0,
    daily_tokens_used INTEGER NOT NULL DEFAULT 0,
    day_window_start TEXT NOT NULL DEFAULT '',
    requires_auth BOOLEAN DEFAULT 0,
    cors_origins TEXT DEFAULT '',
    -- response_schema: when non-empty, a JSON Schema the proxy sets as the
    -- completion's ResponseSchema so the provider emits conforming JSON
    -- (response_format=json_schema). Empty = free-form text.
    response_schema TEXT NOT NULL DEFAULT '',
    -- knowledge_base: binds this proxy endpoint to a RAG knowledge base by name
    -- (of_knowledge_bases.name). When set, the proxy retrieves the most relevant
    -- chunks per request and prepends them to the system prompt. Empty = plain chat.
    knowledge_base TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Per-day token totals archived at window-roll time: the proxy inserts the
-- previous day's final daily_tokens_used when day_window_start rolls over, so
-- manage_analytics can build a time series. Today's running total lives in
-- of_llm_endpoints.daily_tokens_used until the next UTC day archives it.
CREATE TABLE IF NOT EXISTS of_llm_usage_daily (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL,
    day TEXT NOT NULL,
    tokens_used INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(path, day)
);
CREATE INDEX IF NOT EXISTS idx_llm_usage_daily_day ON of_llm_usage_daily(day);

-- Hosted agent endpoints (manage_agent): the multi-step sibling of
-- of_llm_endpoints. One row mounts POST /api/<path>/run, where the runtime
-- (internal/agentrt) runs a bounded tool-calling loop on the project's LLM. The
-- agent's tools are the app's OWN capabilities, allowlisted per endpoint, never
-- the platform build tools.
--   max_steps          Tool-calling iterations before the loop stops (capped in agentrt).
--   functions          JSON array of manage_function endpoint paths the agent may call as tools.
--   allow_data_query   When 1, exposes a read-only SELECT tool guarded like ctx.query.
--   daily_token_budget Per-endpoint per-UTC-day cap on the WHOLE run's input+output tokens
--                      (0 = unlimited); daily_tokens_used / day_window_start are its counter,
--                      rolled forward on the first hit each UTC day (mirrors the LLM proxy).
--   requires_auth      Defaults ON: an agent spends the owner's tokens over several calls, so
--                      a public one is opt-in, unlike the single-shot proxy.
CREATE TABLE IF NOT EXISTS of_agent_endpoints (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    system_prompt TEXT NOT NULL DEFAULT '',
    model_id TEXT DEFAULT '',
    max_tokens INTEGER DEFAULT 4096,
    temperature REAL DEFAULT 0.7,
    max_steps INTEGER NOT NULL DEFAULT 8,
    rate_limit INTEGER DEFAULT 10,
    daily_token_budget INTEGER NOT NULL DEFAULT 0,
    daily_tokens_used INTEGER NOT NULL DEFAULT 0,
    day_window_start TEXT NOT NULL DEFAULT '',
    requires_auth BOOLEAN DEFAULT 1,
    cors_origins TEXT DEFAULT '',
    functions TEXT NOT NULL DEFAULT '',
    allow_data_query BOOLEAN NOT NULL DEFAULT 0,
    -- Sub-agent orchestration: max_subagents is the total spawns a whole run may
    -- make (0 = sub-agents disabled, no spawn_subagent tool); max_depth is how
    -- deep they may nest. allow_http_fetch exposes an SSRF-guarded outbound-fetch
    -- tool. All bounded by ceilings in internal/agentrt.
    max_subagents INTEGER NOT NULL DEFAULT 0,
    max_depth INTEGER NOT NULL DEFAULT 1,
    allow_http_fetch BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Async agent runs (manage_agent, POST /run?async=1): a detached run that
-- outlives the HTTP request, so a slow agent survives a client disconnect. The
-- caller polls GET /run?id=<id> for status and, once done, the result. status is
-- running | done | failed; a row stuck running past the wall-clock cap (e.g. the
-- server restarted mid-run) is reported failed at read time.
CREATE TABLE IF NOT EXISTS of_agent_runs (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'running',
    request TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    stop_reason TEXT NOT NULL DEFAULT '',
    steps INTEGER NOT NULL DEFAULT 0,
    subagents INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_agent_runs_path ON of_agent_runs(path, created_at);

-- Per-page SEO overrides: a row's non-empty columns win over the matching
-- of_files.metadata fields at render time; empty columns fall through.
--   structured_data  Raw JSON-LD emitted verbatim inside <script type="application/ld+json">.
--   twitter_card     summary | summary_large_image | player | app.
--   locale           BCP-47 tag (e.g. "en", "fr-CA"); drives <html lang> and og:locale.
CREATE TABLE IF NOT EXISTS of_seo_meta (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    page_path TEXT UNIQUE NOT NULL,
    title TEXT DEFAULT '',
    description TEXT DEFAULT '',
    og_title TEXT DEFAULT '',
    og_description TEXT DEFAULT '',
    og_image TEXT DEFAULT '',
    canonical_url TEXT DEFAULT '',
    robots TEXT DEFAULT '',
    structured_data TEXT DEFAULT '',
    twitter_card TEXT DEFAULT '',
    locale TEXT DEFAULT '',
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- URL redirects (301/302)
CREATE TABLE IF NOT EXISTS of_redirects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    redirect_from TEXT NOT NULL UNIQUE,
    redirect_to TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 301,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_redirects_from ON of_redirects(redirect_from);

-- WebSocket endpoints
CREATE TABLE IF NOT EXISTS of_ws_endpoints (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    write_to_table TEXT DEFAULT '',
    room_column TEXT DEFAULT '',
    requires_auth BOOLEAN DEFAULT 0,
    -- tick_hz: 0 = per-message relay; > 0 = server-driven tick, holding each
    -- client's latest message and broadcasting one combined snapshot per tick.
    tick_hz INTEGER DEFAULT 0,
    -- rate_limit: inbound text messages/sec per client (0 = unlimited); excess dropped.
    rate_limit INTEGER DEFAULT 120,
    -- max_msg_size: byte cap on one inbound frame; oversized frames are dropped.
    max_msg_size INTEGER DEFAULT 16384,
    -- tenant_column: column on write_to_table auto-stamped from claims.TenantID;
    -- also folded into the room key so tenants sharing a room name stay isolated.
    tenant_column TEXT DEFAULT '',
    -- e2ee: when 1 the relay enforces end-to-end encryption; frames carry an
    -- {iv,ct} envelope, only ciphertext is persisted, and the server holds no key.
    e2ee BOOLEAN DEFAULT 0,
    -- ephemeral: when 1 the relay prunes a room's persisted rows once it empties,
    -- so a throwaway session leaves nothing stored. Needs write_to_table +
    -- room_column; defaults on for e2ee.
    ephemeral BOOLEAN DEFAULT 0,
    -- message_ttl_seconds: when > 0, persisted rows older than this are swept
    -- ("disappearing messages"), lazily on relay activity. 0 = keep forever.
    -- Age-based, independent of ephemeral (occupancy-based); the two compose.
    message_ttl_seconds INTEGER DEFAULT 0,
    -- room_gated: when 1 a peer must present a per-room access token (?join=<token>)
    -- to join a non-global room. The first peer mints it (welcome._joinToken); later
    -- peers without it are refused (401). Defaults on for e2ee signaling endpoints.
    room_gated BOOLEAN DEFAULT 0,
    -- matchmaking: when 1 the relay pairs arriving peers server-side. Once
    -- group_size peers wait in the lobby it mints one shared room, assigns roles,
    -- and pushes each a {_type:"matched",...} frame. Requires room_column;
    -- incompatible with room_gated.
    matchmaking BOOLEAN DEFAULT 0,
    -- group_size: number of peers per match for a matchmaking endpoint (>=2).
    group_size INTEGER DEFAULT 2,
    -- Criteria-based matchmaking tuning (manage_matchmaking): peers pass {rating,
    -- region} in ?meta=; formMatch widens a waiting peer's rating band over time
    -- so nobody starves. match_by = '' keeps plain "claim any group_size peers".
    --   match_by            '' | 'rating' | 'region' | 'rating+region'
    --   rating_band         initial rating half-width for rating matching
    --   band_widen_per_sec  how fast the band grows with the longest-waiting peer's wait
    --   max_wait_secs       past this, that peer matches any rating (region still honored); 0 = never
    match_by TEXT NOT NULL DEFAULT '',
    rating_band INTEGER NOT NULL DEFAULT 200,
    band_widen_per_sec REAL NOT NULL DEFAULT 0,
    max_wait_secs INTEGER NOT NULL DEFAULT 0,
    -- max_clients: cap on participants per room (0 = unlimited). Joiners past the
    -- cap are admitted as read-only spectators (welcome._role="spectator") so a
    -- full game can still be watched. Not applied on matchmaking lobbies.
    max_clients INTEGER DEFAULT 0,
    -- authoritative: when 1 the endpoint is backed by a server-side room actor
    -- (one goroutine per room running `reducer` in a goja sandbox). _action frames
    -- go through reduce(); new state is persisted (of_ws_room_state) and each
    -- player is pushed their view(). Set by manage_authoritative set_reducer.
    authoritative BOOLEAN DEFAULT 0,
    -- model: the realtime tool that created (and owns) the endpoint - 'relay' |
    -- 'tick' | 'authoritative' | 'matchmaking' | 'signaling'. Each model tool's
    -- update refuses another model's endpoint. '' = unknown (inferred from flags).
    model TEXT NOT NULL DEFAULT '',
    -- reducer: model-authored JS (reduce()/optional view()) the room actor runs
    -- sandboxed. Validated by goja on save and on spawn. Never served to a browser.
    reducer TEXT DEFAULT '',
    -- seed_table / seed_key_column / seed_columns: join-seed config. On @join the
    -- room actor reads seed_columns from seed_table WHERE seed_key_column =
    -- <authenticated user id> and injects the row as event.player.seed, so a
    -- reducer seeds durable state (e.g. a bankroll) from a trusted server-side id.
    -- Empty seed_table disables it. seed_columns is a JSON array of column names.
    seed_table TEXT DEFAULT '',
    seed_key_column TEXT DEFAULT '',
    seed_columns TEXT DEFAULT '',
    -- shared_table / shared_columns: optional cross-room read for the reducer.
    -- When set, the room actor reads its rows and injects them as event.shared on
    -- every non-@tick event, so a reducer can read global state (a pooled jackpot,
    -- a config row) while authority stays per-room. Empty = off; shared_columns is
    -- a JSON array ('' = all).
    shared_table TEXT NOT NULL DEFAULT '',
    shared_columns TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- of_ws_room_tokens holds the active per-room access token for room_gated
-- endpoints. The row lives only while the room is occupied (deleted when it
-- empties), so a finished session leaves nothing behind. tenant_id folds in
-- multi-tenant isolation (0 for non-tenanted endpoints).
CREATE TABLE IF NOT EXISTS of_ws_room_tokens (
    endpoint_path TEXT NOT NULL,
    tenant_id     INTEGER NOT NULL DEFAULT 0,
    room          TEXT NOT NULL,
    token         TEXT NOT NULL,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (endpoint_path, tenant_id, room)
);

-- of_ws_room_state holds the authoritative per-room state for actor-backed
-- endpoints, so a room resumes after restart and a late joiner gets current
-- state. rev is a monotonic revision; pruned with the room for ephemeral endpoints.
CREATE TABLE IF NOT EXISTS of_ws_room_state (
    endpoint_path TEXT NOT NULL,
    tenant_id     INTEGER NOT NULL DEFAULT 0,
    room          TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'null',
    rev           INTEGER NOT NULL DEFAULT 0,
    updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (endpoint_path, tenant_id, room)
);

-- Payment configuration (singleton), one-off payments, and the webhook
-- idempotency ledger. billing.ReconcileWebhook claims each verified provider event
-- in of_processed_webhooks before touching a money row, so a replayed delivery
-- cannot double-apply.
CREATE TABLE IF NOT EXISTS of_payment_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    provider_name TEXT NOT NULL,
    provider_type TEXT NOT NULL DEFAULT 'generic',
    currency TEXT NOT NULL DEFAULT 'usd',
    webhook_secret_name TEXT DEFAULT '',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_payments (
    id INTEGER PRIMARY KEY,
    external_id TEXT DEFAULT '',
    amount INTEGER NOT NULL,
    currency TEXT NOT NULL DEFAULT 'usd',
    status TEXT NOT NULL DEFAULT 'pending',
    description TEXT DEFAULT '',
    metadata TEXT DEFAULT '{}',
    checkout_url TEXT DEFAULT '',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_processed_webhooks (
    event_key TEXT PRIMARY KEY,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Subscription plans (recurring billing)
CREATE TABLE IF NOT EXISTS of_subscription_plans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    external_id TEXT DEFAULT '',
    amount INTEGER NOT NULL,
    currency TEXT NOT NULL DEFAULT 'usd',
    interval TEXT NOT NULL DEFAULT 'month',
    trial_days INTEGER DEFAULT 0,
    metadata TEXT DEFAULT '{}',
    is_active BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Subscriptions (user-to-plan bindings)
CREATE TABLE IF NOT EXISTS of_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER,
    plan_id INTEGER REFERENCES of_subscription_plans(id),
    external_id TEXT DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    current_period_start DATETIME,
    current_period_end DATETIME,
    canceled_at DATETIME,
    metadata TEXT DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_subscriptions_user ON of_subscriptions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_of_subscriptions_external ON of_subscriptions(external_id);

-- Server-side triggers (event-driven hooks: send email, HTTP request, insert/update data)
CREATE TABLE IF NOT EXISTS of_triggers (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_filter TEXT,         -- JSON: {"column":"value"} to match against event payload
    trigger_type TEXT NOT NULL, -- send_email, http_request, insert_data, update_data, validate, transform, reject_if, send_notification
    trigger_config TEXT NOT NULL, -- JSON config with {{template}} variables from event payload
    timing TEXT DEFAULT 'after', -- "before" or "after" the event (before runs synchronously and can reject)
    is_enabled BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Roles and permissions
CREATE TABLE IF NOT EXISTS of_roles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT DEFAULT '',
    permissions TEXT DEFAULT '[]',
    is_system BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- No default roles are seeded. Each project defines its own roles (if any) via manage_roles.

-- Notifications
CREATE TABLE IF NOT EXISTS of_notifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER,
    title TEXT NOT NULL,
    message TEXT DEFAULT '',
    type TEXT DEFAULT 'info',
    is_read BOOLEAN DEFAULT 0,
    action_url TEXT DEFAULT '',
    metadata TEXT DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    read_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_of_notifications_user ON of_notifications(user_id, is_read, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_of_notifications_date ON of_notifications(created_at DESC);

-- Email configuration (singleton)
CREATE TABLE IF NOT EXISTS of_email_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    provider_name TEXT NOT NULL,
    provider_type TEXT NOT NULL DEFAULT 'smtp',
    from_address TEXT NOT NULL,
    from_name TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Email templates
CREATE TABLE IF NOT EXISTS of_email_templates (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    subject TEXT NOT NULL DEFAULT '',
    body_html TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Background job queue
CREATE TABLE IF NOT EXISTS of_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    payload TEXT DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'pending',
    max_attempts INTEGER DEFAULT 3,
    attempts INTEGER DEFAULT 0,
    result TEXT DEFAULT '',
    error TEXT DEFAULT '',
    scheduled_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    completed_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_jobs_status ON of_jobs(status, scheduled_at);

-- =============================================================================
-- Extended endpoint surfaces: CSV import/export, 2FA, magic links, feeds,
-- calendars, PDF rendering, web push. Each row is queried per-request by the
-- dynamic router (live reload automatic), same pattern as of_api_endpoints.
-- =============================================================================

-- Runtime PDF endpoints. POST /api/<path>/pdf with {"data": {...}} fills template_html
-- (internal/doctemplate: escaped {{values}} and {{#each}} blocks) and answers the
-- rendered PDF. requires_auth defaults to 1: a document generator is a rendering
-- service, so it is closed unless the project opens it.
CREATE TABLE IF NOT EXISTS of_document_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    template_html TEXT NOT NULL,
    filename TEXT NOT NULL DEFAULT 'document',  -- may use {{values}}; sanitized, .pdf appended
    page_size TEXT NOT NULL DEFAULT 'A4',
    landscape BOOLEAN DEFAULT 0,
    requires_auth BOOLEAN DEFAULT 1,
    rate_limit INTEGER DEFAULT 10,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- CSV / XLSX export endpoints. Reads from table_name, streams CSV at
-- /api/<path>/export. requires_auth applies bearer-token guard.
CREATE TABLE IF NOT EXISTS of_export_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    format TEXT NOT NULL DEFAULT 'csv',          -- csv | xlsx (xlsx not yet wired)
    public_columns TEXT,                          -- JSON array; nil = all non-secure
    requires_auth BOOLEAN DEFAULT 0,
    owner_column TEXT,                            -- row-level scope to authed user
    tenant_column TEXT DEFAULT '',                -- row-level scope to user's active tenant
    rate_limit INTEGER DEFAULT 30,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- CSV import endpoints. POST multipart/form-data file=<csv> at /api/<path>/import.
-- mode: insert | upsert (upsert keys on unique_column).
CREATE TABLE IF NOT EXISTS of_import_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'insert',          -- insert | upsert
    unique_column TEXT,                            -- required when mode='upsert'
    requires_auth BOOLEAN DEFAULT 1,
    max_rows INTEGER DEFAULT 10000,
    tenant_column TEXT DEFAULT '',                -- when set, every imported row is stamped with claims.TenantID
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Read-only raw-SELECT endpoint, opt-in per project (off unless a row exists):
-- POST /api/<path> with {"sql":"SELECT ..."} returns {columns, rows, row_count,
-- execution_ms}. The only runtime surface for joins/aggregations/views. Grants
-- read of every non-system table, so it is enabled explicitly (manage_api
-- enable_query), never implicitly; the SELECT-only guard, row cap, and timeout
-- are enforced by the handler.
CREATE TABLE IF NOT EXISTS of_query_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE DEFAULT '_query',
    max_rows INTEGER NOT NULL DEFAULT 200,
    requires_auth BOOLEAN DEFAULT 0,
    rate_limit INTEGER DEFAULT 60,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- TOTP 2FA enablement. One row per auth endpoint that has TOTP enabled.
-- Per-user secrets live in of_user_totp.
CREATE TABLE IF NOT EXISTS of_totp_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,                    -- the auth endpoint path TOTP attaches to
    auth_endpoint_path TEXT NOT NULL,
    issuer TEXT NOT NULL DEFAULT '',              -- shown in authenticator apps
    enforced BOOLEAN DEFAULT 0,                   -- when 1, login requires verified TOTP
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_user_totp (
    user_id INTEGER PRIMARY KEY,
    auth_endpoint_path TEXT NOT NULL,
    secret_encrypted TEXT NOT NULL,
    verified BOOLEAN DEFAULT 0,
    backup_codes_encrypted TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    verified_at DATETIME
);

-- Magic-link passwordless login. One row per auth endpoint with magic links on.
CREATE TABLE IF NOT EXISTS of_magic_link_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    auth_endpoint_path TEXT NOT NULL,
    email_template TEXT NOT NULL DEFAULT '',      -- of_email_templates.name; falls back to inline default
    redirect_url TEXT NOT NULL DEFAULT '/',
    ttl_minutes INTEGER NOT NULL DEFAULT 15,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_magic_tokens (
    token TEXT PRIMARY KEY,
    auth_endpoint_path TEXT NOT NULL,
    email TEXT NOT NULL,
    user_id INTEGER,                              -- nil until first redeem creates the user
    expires_at DATETIME NOT NULL,
    used BOOLEAN DEFAULT 0,
    -- purpose: what redeeming the token does. 'login' is a magic-link sign-in,
    -- 'reset' lets the holder choose a new password, 'totp_enroll' and 'totp_login'
    -- stand in for a session at /totp/setup and /totp/verify while a sign-in waits on
    -- its second factor. Never interchangeable.
    purpose TEXT NOT NULL DEFAULT 'login',
    -- method: for a totp_* token, the sign-in it completes (password | oauth |
    -- magic_link), carried onto the auth.login event.
    method TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_magic_tokens_email ON of_magic_tokens(email, expires_at);

-- RSS / Atom feed endpoints. GET /feed/<path> renders rows from table_name.
CREATE TABLE IF NOT EXISTS of_feed_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    format TEXT NOT NULL DEFAULT 'rss',           -- rss | atom
    title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    link TEXT NOT NULL DEFAULT '/',
    title_column TEXT NOT NULL DEFAULT 'title',
    body_column TEXT NOT NULL DEFAULT 'body',
    link_column TEXT,                              -- if set, used to construct per-item link
    date_column TEXT NOT NULL DEFAULT 'created_at',
    item_limit INTEGER NOT NULL DEFAULT 50,
    where_json TEXT NOT NULL DEFAULT '[]',        -- structured WHERE filter: JSON array of {column, op, value}
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);


-- Web Push (VAPID). One row per /api/<path>/push endpoint, plus per-user subs.
CREATE TABLE IF NOT EXISTS of_push_endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    vapid_public_key TEXT NOT NULL,
    vapid_private_secret_name TEXT NOT NULL,       -- ref into of_secrets
    contact_email TEXT NOT NULL DEFAULT '',
    requires_auth BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS of_push_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint_path TEXT NOT NULL,
    user_id INTEGER,
    endpoint_url TEXT NOT NULL,
    p256dh TEXT NOT NULL,
    auth_key TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(endpoint_url)
);

CREATE INDEX IF NOT EXISTS idx_of_push_subs_path ON of_push_subscriptions(endpoint_path);
CREATE INDEX IF NOT EXISTS idx_of_push_subs_user ON of_push_subscriptions(user_id);

-- of_endpoints is the authoritative registry of what is mounted on the public
-- /api/<path> namespace: one row per route, keyed (path, sub_segment, method).
--   sub_segment = '' for a bare-path owner (/api/<path>): crud, function, query.
--   sub_segment = the reserved suffix for a sub-route (/api/<path>/<seg>):
--                 upload, stream, chat, complete, ws, rooms, export, import, ...
--   method      = the HTTP method, or '*' when it is not the discriminator.
-- UNIQUE(path, sub_segment, method) blocks cross-kind collisions while allowing
-- method-aware co-location. Runtime dispatch resolves the owning kind here; the
-- per-kind of_*_endpoints tables hold only kind-specific config.
CREATE TABLE IF NOT EXISTS of_endpoints (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT NOT NULL,
    sub_segment TEXT NOT NULL DEFAULT '',
    method      TEXT NOT NULL DEFAULT '*',
    kind        TEXT NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(path, sub_segment, method)
);

-- Additional indexes for frequently queried columns
CREATE INDEX IF NOT EXISTS idx_of_endpoints_lookup ON of_endpoints(path, sub_segment);
CREATE INDEX IF NOT EXISTS idx_of_api_endpoints_path ON of_api_endpoints(path);
CREATE INDEX IF NOT EXISTS idx_of_auth_endpoints_path ON of_auth_endpoints(path);
CREATE INDEX IF NOT EXISTS idx_of_dynamic_tables_name ON of_dynamic_tables(table_name);
CREATE INDEX IF NOT EXISTS idx_of_webhooks_name ON of_webhooks(name);
CREATE INDEX IF NOT EXISTS idx_of_webhooks_direction ON of_webhooks(direction);
CREATE INDEX IF NOT EXISTS idx_of_scheduled_tasks_name ON of_scheduled_tasks(name);

-- Multi-tenancy (row-level SaaS isolation). of_tenants is the registry of tenant
-- workspaces inside a project; user-facing rows carry a tenant_id referencing
-- id here, and of_tenant_members joins users to tenants with a per-tenant role.
-- Opt-in (manage_tenancy enable): projects with no tenant_column set are unaffected.

CREATE TABLE IF NOT EXISTS of_tenants (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    slug TEXT NOT NULL UNIQUE,                    -- URL-safe identifier, used in subdomain or path routing later
    name TEXT NOT NULL,
    plan TEXT DEFAULT 'free',                     -- free | pro | enterprise | <project-defined>
    settings TEXT DEFAULT '{}',                   -- JSON: per-tenant config (branding, limits, feature flags)
    is_deleted BOOLEAN DEFAULT 0,
    deleted_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_tenants_slug ON of_tenants(slug);

CREATE TABLE IF NOT EXISTS of_tenant_members (
    tenant_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role TEXT NOT NULL DEFAULT 'member',          -- owner | admin | member | viewer | <project-defined>
    status TEXT NOT NULL DEFAULT 'active',        -- active | invited | disabled
    invited_by INTEGER,
    joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, user_id),
    FOREIGN KEY (tenant_id) REFERENCES of_tenants(id)
);

CREATE INDEX IF NOT EXISTS idx_of_tenant_members_user ON of_tenant_members(user_id);

-- of_tenant_invites holds outstanding invitations for invite_only signup mode.
-- The token is presented on /api/<auth_path>/register?invite=<token>; single-use,
-- deleted on consumption.
CREATE TABLE IF NOT EXISTS of_tenant_invites (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id INTEGER NOT NULL,
    email TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'member',
    token TEXT NOT NULL UNIQUE,
    invited_by INTEGER,
    expires_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (tenant_id) REFERENCES of_tenants(id)
);

CREATE INDEX IF NOT EXISTS idx_of_tenant_invites_token ON of_tenant_invites(token);
CREATE INDEX IF NOT EXISTS idx_of_tenant_invites_email ON of_tenant_invites(email);

-- =============================================================================
-- Persistent symbol graph: a cross-file definition + reference index maintained
-- on every artifact write (store.IndexArtifactSymbols), keyed by a body
-- fingerprint so it stays fresh. Lets the builder resolve a symbol straight to
-- file:line (manage_codebase resolve / where_used) instead of grepping. `kind`
-- is a free string (js_func, css_selector, html_id, ...), not branched on.
-- =============================================================================

-- One row per defined symbol. (file_kind, file_path) name the artifact the
-- symbol lives in (the of_files discriminator + path); line is 1-indexed.
CREATE TABLE IF NOT EXISTS of_symbols (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    file_kind TEXT NOT NULL,
    file_path TEXT NOT NULL,
    line INTEGER NOT NULL DEFAULT 0,
    signature TEXT NOT NULL DEFAULT '',
    exported INTEGER NOT NULL DEFAULT 0,
    fingerprint TEXT NOT NULL DEFAULT '',
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_symbols_name ON of_symbols(name);
CREATE INDEX IF NOT EXISTS idx_symbols_file ON of_symbols(file_kind, file_path);
CREATE INDEX IF NOT EXISTS idx_symbols_kind ON of_symbols(kind);

-- One row per usage edge: a reference to symbol_name made from
-- (ref_file_kind, ref_file_path). target_path is the resolved defining path when
-- the reference is a module import the resolver can pin, else empty.
CREATE TABLE IF NOT EXISTS of_symbol_refs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol_name TEXT NOT NULL,
    ref_file_kind TEXT NOT NULL,
    ref_file_path TEXT NOT NULL,
    ref_line INTEGER NOT NULL DEFAULT 0,
    ref_kind TEXT NOT NULL DEFAULT '',
    target_path TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL DEFAULT '',
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_symbol_refs_name ON of_symbol_refs(symbol_name);
CREATE INDEX IF NOT EXISTS idx_symbol_refs_file ON of_symbol_refs(ref_file_kind, ref_file_path);

-- Per-file index state: the fingerprint the rows above were extracted from, so a
-- re-save of an unchanged body short-circuits re-extraction and lazy backfill can
-- find what still needs indexing.
CREATE TABLE IF NOT EXISTS of_symbol_index_state (
    file_kind TEXT NOT NULL,
    file_path TEXT NOT NULL,
    fingerprint TEXT NOT NULL DEFAULT '',
    indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (file_kind, file_path)
);

-- =============================================================================
-- Engine build loop. of_runs + of_run_log + of_llm_calls + of_llm_cache +
-- of_tasks + of_sessions are the engine's durable state.
-- =============================================================================

-- of_runs: one row per pipeline run (a full build, or a LIVE update cycle).
-- The most recent row is the project's current/last run; an active run is the
-- single row with status IN ('running','paused').
CREATE TABLE IF NOT EXISTS of_runs (
    id          TEXT PRIMARY KEY,                 -- run_<unixms>_<seq>
    kind        TEXT NOT NULL DEFAULT 'build',    -- build | update
    status      TEXT NOT NULL DEFAULT 'running',  -- running | paused | completed | failed
    stage       TEXT NOT NULL DEFAULT 'plan',     -- current stage name (registry key)
    plan_json   TEXT,
    update_description TEXT,
    -- expanded_brief: the EXPAND stage's sharpened brief, read by PLAN and BUILD.
    expanded_brief TEXT NOT NULL DEFAULT '',
    -- pause_reason: awaiting_owner_answers | awaiting_approval | failure text.
    -- Manual owner pause lives on projects.mode (global DB), not here.
    pause_reason TEXT,
    error_count INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT,
    total_input_tokens  INTEGER NOT NULL DEFAULT 0,
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    total_cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    total_cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    started_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    -- engine_version / engine_built_at: the engine binary that produced this run,
    -- written at run creation from main.Version / main.BuildTime so a post-mortem
    -- reads "which binary ran this" as one field.
    engine_version TEXT NOT NULL DEFAULT '',
    engine_built_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_of_runs_status ON of_runs(status, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_of_runs_started ON of_runs(started_at DESC);

-- of_run_log: unified append-only journal per run. id is the sequence; readers
-- ORDER BY id.
--   kind  stage | task | tool | message | issue | session
--   ref   stage name / task id / tool name / session id (kind-dependent)
CREATE TABLE IF NOT EXISTS of_run_log (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    TEXT NOT NULL DEFAULT '',
    kind      TEXT NOT NULL,
    ref       TEXT NOT NULL DEFAULT '',
    payload   TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_run_log_run ON of_run_log(run_id, id);
CREATE INDEX IF NOT EXISTS idx_of_run_log_kind ON of_run_log(kind, id);

-- of_spend_limits: this project's own daily AI spend limits (singleton row).
-- daily_token_budget caps the unattended pool (engine, triggers, jobs, functions;
-- NULL = the instance default), daily_public_token_budget the public pool (LLM proxy
-- and agent endpoints; NULL = llm.DefaultPublicDailyTokenBudget), and daily_usd_cap
-- (NULL = none) is one dollar ceiling over both. Read by llm.CheckSpend and
-- llm.CheckPublicSpend before every billable model call.
CREATE TABLE IF NOT EXISTS of_spend_limits (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    daily_token_budget INTEGER,
    daily_public_token_budget INTEGER,
    daily_usd_cap REAL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- of_llm_calls: one row per provider call (llm.DBLLMLogger). request_hash is the
-- canonical request fingerprint (the of_llm_cache key); cache_hit=1 marks a call
-- served from of_llm_cache without hitting the provider.
CREATE TABLE IF NOT EXISTS of_llm_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source TEXT NOT NULL,                          -- engine | trigger | job | function | chat | proxy | agent
    run_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',           -- engine session segment id / chat session id / proxy endpoint path
    stage TEXT NOT NULL DEFAULT '',                -- plan | build | live; empty for chat
    iteration INTEGER NOT NULL DEFAULT 0,
    model TEXT NOT NULL,
    -- provider_type is the WIRE PROTOCOL (openai-chat | anthropic | ollama),
    -- provider_name the row that was called.
    -- Both, because most vendors share one transport: the type alone cannot
    -- tell an OpenRouter call from a Z.ai one.
    provider_type TEXT NOT NULL DEFAULT '',
    provider_name TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    cache_hit INTEGER NOT NULL DEFAULT 0,
    request_messages TEXT,
    request_system TEXT,
    request_tools TEXT,
    request_max_tokens INTEGER,
    response_content TEXT,
    response_tool_calls TEXT,
    response_stop_reason TEXT,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    -- Prefix-stability diagnostics: bytes/messages of this request byte-identical
    -- to the previous call in the same segment (what prompt caches key on). A
    -- collapse alongside a cache_read drop pinpoints a cache-breaking history rewrite.
    stable_prefix_bytes INTEGER NOT NULL DEFAULT 0,
    stable_prefix_msgs  INTEGER NOT NULL DEFAULT 0,
    total_request_bytes INTEGER NOT NULL DEFAULT 0,
    -- request_tool_bytes is the serialized size of the tool schemas. The tool block
    -- rides the cached prefix, so it is re-read on every turn; it is the largest
    -- prefix component and without this column the only un-budgeted one.
    request_tool_bytes INTEGER NOT NULL DEFAULT 0,
    -- tool_prefix_changed marks the call whose tool block differs from the previous
    -- call in the same segment (a mid-session find_tool load). That reserializes the
    -- block the provider cache keyed on, so the whole prefix re-bills at full price.
    tool_prefix_changed INTEGER NOT NULL DEFAULT 0,
    -- duration_ms is the WHOLE call including retry backoff; retry_wait_ms is how
    -- much of it was sleeping. Provider time is the difference. Without the split a
    -- throttled call and a slow one are indistinguishable.
    duration_ms INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    retry_wait_ms INTEGER NOT NULL DEFAULT 0,
    used_fallback INTEGER NOT NULL DEFAULT 0,
    error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_llm_calls_run ON of_llm_calls(run_id, id);
CREATE INDEX IF NOT EXISTS idx_of_llm_calls_session ON of_llm_calls(session_id, id);
CREATE INDEX IF NOT EXISTS idx_of_llm_calls_hash ON of_llm_calls(request_hash);
CREATE INDEX IF NOT EXISTS idx_of_llm_calls_date ON of_llm_calls(created_at DESC);

-- of_llm_cache: content-addressed replay cache keyed by the SHA-256 of the
-- canonical request bytes (provider_type, model, system, messages, tools,
-- max_tokens, temperature, thinking, response_schema). An identical engine state
-- replays the stored response for free (crash resume / retries cost ~0 tokens).
-- Pruned by last_hit_at via Engine.StartMaintenance.
CREATE TABLE IF NOT EXISTS of_llm_cache (
    request_hash TEXT PRIMARY KEY,
    response_json TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    hits INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_hit_at DATETIME
);

-- of_tasks: the engine's durable build checklist, one row per plan item. The
-- build session ticks rows off as it works; a row's status IS the crash-recovery
-- checkpoint (done rows stay checked on resume, the rest re-queue).
CREATE TABLE IF NOT EXISTS of_tasks (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL,
    intent      TEXT NOT NULL DEFAULT '',
    done_when   TEXT,
    status      TEXT NOT NULL DEFAULT 'pending',  -- pending | active | done | blocked
    deps        TEXT,                             -- JSON array of task ids
    paths       TEXT,                             -- JSON array of owned artifact paths
    -- edits: paths a task MODIFIES but another task OWNS (cross-module edits).
    -- The build seed lists these as the task's Extends set so it modifies them in
    -- place rather than re-creating them.
    edits       TEXT,                             -- JSON array of cross-edit paths
    -- tools: non-core tool names the PLANNER declared this task needs. Preloaded
    -- into the build session so planned work never needs a mid-session find_tool,
    -- which would append a tool schema and bust the provider's prompt-prefix cache.
    tools       TEXT,                             -- JSON array: planner-declared tool needs
    -- exports: named symbols this task's module(s) publish for sibling tasks to
    -- import (the planner-declared leaf interface). Surfaced in the build checklist
    -- so a consumer imports the accessor instead of stubbing the gap on window.__x.
    exports     TEXT,                             -- JSON array: planner-declared export interface
    origin      TEXT,                             -- plan | repair | discovered | split
    -- blocked_on: who can unblock a blocked item. 'owner' when only the owner's answer or
    -- secret unblocks it (an owner's answer reopens exactly these); '' for any other block.
    blocked_on  TEXT NOT NULL DEFAULT '',
    result      TEXT,
    conventions TEXT NOT NULL DEFAULT '',
    position    INTEGER DEFAULT 0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_of_tasks_status ON of_tasks(status, position);
CREATE INDEX IF NOT EXISTS idx_of_tasks_run ON of_tasks(run_id, position);

-- of_sessions: segment metadata for engine LLM sessions. A segment is one
-- append-only transcript; context overflow or crash ends it and the next reseeds
-- deterministically from DB state. Transcript bodies live in of_llm_calls, not here.
CREATE TABLE IF NOT EXISTS of_sessions (
    id         TEXT PRIMARY KEY,                  -- sess_<unixms>_<n>
    run_id     TEXT NOT NULL DEFAULT '',
    segment    INTEGER NOT NULL DEFAULT 1,
    end_reason TEXT NOT NULL DEFAULT '',          -- done | checklist_complete | iteration_cap | read_only_streak | segments_exhausted | tool_pause | approval | owner_stop | wedged | continuation_cap | repeat_stall | error | model_lacks_tools
    iterations INTEGER NOT NULL DEFAULT 0,
    started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    ended_at   DATETIME
);

CREATE INDEX IF NOT EXISTS idx_of_sessions_run ON of_sessions(run_id, started_at);

-- of_flow_budget: the user-flow replay budget and stall state, one row per run.
-- It keeps internal/replaybudget's counters in the DB rather than process memory,
-- so the cap survives the build->repair process restart and crash-resume: a run
-- keeps one row and its own count, where an in-memory counter handed the model a
-- fresh allowance on every restart (a build could replay far past Max; see
-- docs/TRIAGE.md). replays is the budget consumed; the rest mirror the stall
-- tracker's per-run state. A green full run clears the stall fields but never
-- refunds replays.
CREATE TABLE IF NOT EXISTS of_flow_budget (
    run_id        TEXT PRIMARY KEY,
    replays       INTEGER NOT NULL DEFAULT 0,  -- replays taken against this run
    best          INTEGER NOT NULL DEFAULT -1, -- fewest failed flows in a red full run; -1 before the first
    best_depth    INTEGER NOT NULL DEFAULT 0,  -- furthest the failing flows got at that count
    partial_depth INTEGER NOT NULL DEFAULT 0,  -- furthest a red single-flow replay got since the last full run
    stall_count   INTEGER NOT NULL DEFAULT 0,  -- replays in a row without progress
    red           INTEGER NOT NULL DEFAULT 0,  -- 1 when the latest replay failed
    updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- of_edit_failures: patch-failure counts for a run, one row per (run, category, key).
-- It keeps internal/editbudget's counters in the DB rather than process memory, so a
-- stuck edit loop is caught across the build->repair restart and crash-resume: the
-- in-memory ReadTracker maps this backs reset on every segment rollover and restart,
-- which let a model re-send the same no-op patch past the STOP meant to break it (see
-- docs/TRIAGE.md, the same bug of_flow_budget fixed for replays). category is 'anchor'
-- (a patch that could not be applied; cleared by a successful write, so count is a
-- streak) or 'noop' (replace equals search; never cleared, so count is the run total
-- of wasted no-change patches). key namespaces by artifact (kind + path).
CREATE TABLE IF NOT EXISTS of_edit_failures (
    run_id     TEXT NOT NULL,
    category   TEXT NOT NULL,            -- 'anchor' or 'noop'
    key        TEXT NOT NULL,            -- kind + path of the artifact
    count      INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, category, key)  -- also covers the (run_id, category) SUM scan
);

-- Platform guides are read-only repo content (knowledge/guides/); there is no project
-- guides overlay. Project-specific knowledge lives in of_memory (see manage_memory).

-- =============================================================================
-- Knowledge bases for retrieval-augmented chat (RAG). A base is a chunk table
-- (kb_<name>) with a vector index, populated by manage_knowledge ingest. An LLM
-- proxy endpoint binds to one via of_llm_endpoints.knowledge_base and prepends
-- the most relevant chunks to the system prompt per request.
-- =============================================================================
CREATE TABLE IF NOT EXISTS of_knowledge_bases (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
