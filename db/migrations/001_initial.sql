-- Global schema: system-wide tables only.
-- Per-project tables live in individual project databases (data/projects/{id}/project.db).

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'admin',
    -- token_version is embedded in each issued JWT. Bumping it (on password change
    -- or "sign out everywhere") invalidates every previously issued token, so a
    -- stolen token cannot outlive a password reset.
    token_version INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Per-user preferences that are not part of the account itself.
--
-- assistant_language is the language the user wants the assistant to write to them
-- in: '' follows the interface language, 'match' answers in the language they write,
-- anything else is a language code. A user with no row has the default ('').
-- Kept out of users so that table stays the credentials/identity record.
CREATE TABLE IF NOT EXISTS user_preferences (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    assistant_language TEXT NOT NULL DEFAULT '',
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    domain TEXT UNIQUE,
    description TEXT,
    -- published: 1 when visitors are served the site, 0 when they see "not found".
    -- Independent of mode: pausing the builder leaves a published site up.
    published INTEGER NOT NULL DEFAULT 0,
    mode TEXT NOT NULL DEFAULT 'building',
    llm_model_id INTEGER NOT NULL,
    config TEXT DEFAULT '{}',
    source_project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    -- ambition_level: how expansive the EXPAND and PLAN stages are. One of 'off'
    -- (skip EXPAND; PLAN at the lean budget), 'low', 'medium', 'high'.
    -- Captured at creation time (default 'high').
    ambition_level TEXT NOT NULL DEFAULT 'high',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS providers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    -- vendor: stable identity for the upstream service (e.g. 'anthropic',
    -- 'ollama', 'pexels'). Used to match seed.json templates against existing
    -- rows on every boot.
    vendor TEXT NOT NULL,
    -- transport: wire protocol used to talk to the provider; the factory looks up
    -- the constructor by this key. Decoupled from vendor so multiple vendors can
    -- share a transport (most vendors speak 'openai-chat').
    transport TEXT NOT NULL,
    api_key_encrypted TEXT,
    -- base_url: the API ROOT, never a full operation endpoint. Store
    -- 'https://api.openai.com/v1', not '.../v1/chat/completions'; the transport
    -- appends the path. Uniform across every provider so sibling endpoints
    -- (/models, /embeddings) are derived by joining rather than recovered by
    -- trimming a suffix that may not be there.
    base_url TEXT,
    -- models_url: catalog endpoint. AUTHORITATIVE when set, derived from
    -- base_url when empty - so a provider that lists on an unrelated path
    -- (a local runtime's own tag listing) is a config row rather than
    -- a Go package. Empty does NOT mean "no sync support"; that is decided by
    -- whether the transport implements ModelLister.
    models_url TEXT NOT NULL DEFAULT '',
    is_enabled BOOLEAN DEFAULT 1,
    -- requires_api_key: when 0, the provider serves without an API key (e.g. local
    -- Ollama). A provider is "available" iff requires_api_key=0 OR api_key_encrypted
    -- IS NOT NULL.
    requires_api_key BOOLEAN NOT NULL DEFAULT 1,
    -- kind: 'llm', 'image', or 'stock'. Declared explicitly in seed.json
    -- rather than inferred from package registration, so a single provider
    -- list can drive chat/image/stock pickers via a column filter.
    kind TEXT NOT NULL DEFAULT 'llm',
    -- plan: how the endpoint is billed. 'standard' is the metered API; 'coding'
    -- is a subscription plan endpoint that vendors usually restrict to
    -- interactive coding tools. Declared in seed.json and shown as a badge.
    plan TEXT NOT NULL DEFAULT 'standard',
    -- signup_url: public URL where the user obtains an API key. Surfaced in
    -- the setup wizard and Add/Edit modal so non-technical users do not
    -- have to guess where to register. Empty for keyless providers.
    signup_url TEXT NOT NULL DEFAULT '',
    -- config: the provider's behaviour as DATA (see internal/providercfg).
    -- Credential placement, static per-vendor headers, prompt-cache opt-in, the
    -- model-listing response shape, and the embedding model all live here, so
    -- adding a vendor that speaks an already-implemented wire protocol is one
    -- row and never a new transport.
    config TEXT DEFAULT '{}',
    -- is_default: per-kind default flag. When a kind-scoped tool (manage_image_gen,
    -- manage_stockphotos) names no provider, the resolver prefers is_default=1 over
    -- the first-by-id fallback. LLM defaults live at model level, so this only does
    -- work for image/stock kinds. At most one per kind (SetProviderAsDefaultForKind).
    is_default BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(vendor)
);

CREATE TABLE IF NOT EXISTS llm_models (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    -- context_tokens / max_output_tokens replace a single max_tokens column
    -- that meant the per-response OUTPUT cap on some providers but the
    -- CONTEXT WINDOW on others. Nothing could read it
    -- without mixing the two, so nothing did. Split, both are usable: the
    -- window bounds what may be sent, the output cap what may be asked for.
    -- 0 = unknown; a model sync fills them when the provider reports them.
    context_tokens INTEGER NOT NULL DEFAULT 0,
    max_output_tokens INTEGER NOT NULL DEFAULT 0,
    supports_streaming BOOLEAN DEFAULT 1,
    supports_tools BOOLEAN DEFAULT 1,
    supports_vision BOOLEAN NOT NULL DEFAULT 0,
    supports_reasoning BOOLEAN NOT NULL DEFAULT 0,
    -- cost_*: USD per MILLION tokens, as reported by the provider or entered by
    -- an admin. Used to weight the daily token budget so a cache-heavy run is
    -- not misread as expensive. 0 = unknown, which falls back to a per-transport estimate.
    cost_input REAL NOT NULL DEFAULT 0,
    cost_output REAL NOT NULL DEFAULT 0,
    cost_cache_read REAL NOT NULL DEFAULT 0,
    cost_cache_write REAL NOT NULL DEFAULT 0,
    is_default BOOLEAN DEFAULT 0,
    -- is_enabled: per-model toggle. When 0 the model is hidden from project-creation
    -- pickers but stays in the DB so projects pinned to it keep working. Provider-level
    -- is_enabled still wins: disabling a provider hides all its models.
    is_enabled BOOLEAN NOT NULL DEFAULT 1,
    config TEXT DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider_id, model_id)
);

CREATE TABLE IF NOT EXISTS bot_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    user_id TEXT DEFAULT '',
    active_project_id INTEGER DEFAULT 0,
    muted INTEGER DEFAULT 0,
    pending_action TEXT NOT NULL DEFAULT '',
    pending_action_at DATETIME,
    pending_data TEXT NOT NULL DEFAULT '',
    last_status_message_id INTEGER NOT NULL DEFAULT 0,
    last_status_at DATETIME,
    last_status_text TEXT NOT NULL DEFAULT '',
    -- quiet | normal | verbose: how much the bot narrates to this person.
    verbosity TEXT NOT NULL DEFAULT 'normal',
    -- The person's live status card message, so a restart keeps editing it.
    live_card_message_id INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider, chat_id)
);

-- Platform guidance lives in the embedded knowledge tree (knowledge/),
-- read-only at runtime - there is no guides table. Project-specific knowledge
-- lives in each project's of_memory (see the project migrations).

-- Global, operator-curated registry of reusable external connections, shared
-- across projects. A project opts in per-entry via of_enabled_connectors; the
-- engine resolves project-local definitions first, then these. Credentials are
-- stored inline, encrypted with the system key.
CREATE TABLE IF NOT EXISTS global_connectors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    base_url TEXT NOT NULL,
    auth_type TEXT NOT NULL DEFAULT 'bearer',
    auth_header TEXT NOT NULL DEFAULT 'Authorization',
    auth_prefix TEXT NOT NULL DEFAULT 'Bearer',
    api_key_encrypted TEXT,
    description TEXT NOT NULL DEFAULT '',
    api_docs TEXT NOT NULL DEFAULT '',
    allow_private_host INTEGER NOT NULL DEFAULT 0,
    is_enabled BOOLEAN DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Audit trail of admin-panel mutations: one row per state-changing request (any
-- method other than GET) that reaches an authenticated admin route. The control
-- plane can read and decrypt every project's data, so a durable who-did-what
-- record is a baseline operational safeguard and the first place to look after an
-- unexpected change. Rows are written best-effort after the handler runs, off the
-- action's critical path, so a failed insert costs an audit row but never the
-- action itself. Unprefixed to match the other global-DB tables (the of_ prefix
-- is reserved for per-project tables).
CREATE TABLE IF NOT EXISTS admin_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    -- user_id/username: the acting admin, taken from the request's JWT claims.
    -- Kept as a denormalized username copy too, so the trail stays readable after
    -- a user row is deleted or renamed.
    user_id INTEGER NOT NULL DEFAULT 0,
    username TEXT NOT NULL DEFAULT '',
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    -- target_project_id: the {projectID} the route acted on, or 0 for a global
    -- action (users, providers, settings, ...).
    target_project_id INTEGER NOT NULL DEFAULT 0,
    status INTEGER NOT NULL DEFAULT 0,
    -- request_id ties the row to the X-Request-ID response header and the
    -- structured log line for the same request.
    request_id TEXT NOT NULL DEFAULT '',
    ip TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- The audit view pages newest-first; this keeps that ordering cheap as the log grows.
CREATE INDEX IF NOT EXISTS idx_admin_audit_created ON admin_audit(created_at DESC, id DESC);
