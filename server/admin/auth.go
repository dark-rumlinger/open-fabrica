package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

// AuthHandler handles authentication endpoints.
type AuthHandler struct {
	deps *Deps
	// lockout throttles repeated failed logins per username, on top of the
	// per-IP login rate limiter, so a distributed/IP-rotating guess against a
	// known admin username is bounded.
	lockout *middleware.AccountLockout
	// setupToken guards first-run setup when the admin panel is bound off
	// loopback: whoever reaches an exposed, unclaimed instance first would
	// otherwise become its admin. It is printed to the server console, so only
	// someone who can read that console can claim the box. Empty (never set or
	// already consumed) means no token is required.
	setupToken atomic.Pointer[string]
}

const setupTokenHeader = "X-Setup-Token"

// armSetupToken generates the one-time setup token for an exposed instance that
// has no admin yet, and logs it. A loopback-bound instance needs none: only a
// local user can reach it.
func (h *AuthHandler) armSetupToken() {
	if h.deps.Config == nil || !h.deps.Config.AdminExposed() || h.deps.DB == nil {
		return
	}
	if n, err := models.CountUsers(h.deps.DB.DB); err != nil || n > 0 {
		return
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		h.deps.Logger.Error("setup token: random source failed; leaving setup unguarded", "error", err)
		return
	}
	tok := hex.EncodeToString(buf)
	h.setupToken.Store(&tok)
	h.deps.Logger.Warn("admin panel is reachable from the network and has no admin account yet: "+
		"enter this setup token in the first-run wizard to claim it", "setup_token", tok)
}

// requireSetupToken reports whether the request may use the first-run setup
// endpoints, writing 403 when a token is armed and the request lacks it. 403, not
// 401: the admin client treats 401 as an expired session and redirects to login.
func (h *AuthHandler) requireSetupToken(w http.ResponseWriter, r *http.Request) bool {
	want := h.setupToken.Load()
	if want == nil || *want == "" {
		return true
	}
	got := r.Header.Get(setupTokenHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(*want)) != 1 {
		writeError(w, http.StatusForbidden, "setup token required: use the token printed in the server console")
		return false
	}
	return true
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string   `json:"token"`
	User  userView `json:"user"`
}

// userView is the explicit allowlist of user fields returned to the client.
// Avoids leaking new fields if models.User grows internally; safer than
// trusting json:"-" tags to be applied to every future sensitive field.
type userView struct {
	ID          int       `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUserView(u *models.User) userView {
	return userView{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		CreatedAt:   u.CreatedAt,
	}
}

// HandleLogin validates credentials and returns a JWT.
func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	lockKey := strings.ToLower(strings.TrimSpace(req.Username))
	if locked, retryAfter := h.lockout.Locked(lockKey); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}

	user, err := models.GetUserByUsername(h.deps.DB.DB, req.Username)
	if err != nil {
		// Run a dummy compare so a missing user takes the same time as a wrong
		// password, denying a username-enumeration timing oracle.
		security.DummyPasswordCompare()
		h.lockout.Fail(lockKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if !security.CheckPassword(req.Password, user.PasswordHash) {
		h.lockout.Fail(lockKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	h.lockout.Reset(lockKey)

	token, err := h.deps.JWTManager.Generate(security.TokenRequest{UserID: user.ID, Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion})
	if err != nil {
		h.deps.serverError(w, "failed to generate token", err)
		return
	}

	h.deps.JWTManager.SetAuthCookie(w, token)

	writeJSON(w, http.StatusOK, loginResponse{
		Token: token,
		User:  toUserView(user),
	})
}

type setupRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
	ProviderID  int    `json:"provider_id,omitempty"`
	ModelID     string `json:"model_id,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	// EngineDailyTokenBudget is the per-project per-UTC-day engine token ceiling
	// chosen in the wizard. A pointer so "not sent" (nil, keep the resolved
	// default) is distinct from an explicit 0 (unlimited, e.g. a free local
	// provider). Persisted to the settings table so the engine picks it up live.
	EngineDailyTokenBudget *int `json:"engine_daily_token_budget,omitempty"`
}

type setupResponse struct {
	Token string   `json:"token"`
	User  userView `json:"user"`
}

// HandleSetupCheck returns whether first-time setup is needed (no users exist).
// This endpoint is public (no JWT required) so the frontend can detect first-run.
func (h *AuthHandler) HandleSetupCheck(w http.ResponseWriter, r *http.Request) {
	count, err := models.CountUsers(h.deps.DB.DB)
	if err != nil {
		h.deps.Logger.Error("setup check: failed to count users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to count users")
		return
	}
	needsSetup := count == 0
	writeJSON(w, http.StatusOK, map[string]bool{
		"needs_setup":       needsSetup,
		"needs_setup_token": needsSetup && h.setupToken.Load() != nil,
	})
}

// setupProviderResponse is the shape returned by HandleProviderCatalog and
// Catalog. IsAvailable is the canonical "can a project use this right now"
// flag, enabled, and either keyless or has a key configured. The frontend
// uses it to render the status pill ("Available" / "Needs key" / "Disabled").
type setupProviderResponse struct {
	ID             int                  `json:"id"`
	Name           string               `json:"name"`
	Vendor         string               `json:"vendor"`
	Transport      string               `json:"transport"`
	Plan           string               `json:"plan"`
	IsEnabled      bool                 `json:"is_enabled"`
	RequiresAPIKey bool                 `json:"requires_api_key"`
	HasAPIKey      bool                 `json:"has_api_key"`
	IsAvailable    bool                 `json:"is_available"`
	SignupURL      string               `json:"signup_url"`
	Models         []setupModelResponse `json:"models"`
}

type setupModelResponse struct {
	ID             int    `json:"id"`
	ModelID        string `json:"model_id"`
	DisplayName    string `json:"display_name"`
	IsDefault      bool   `json:"is_default"`
	IsEnabled      bool   `json:"is_enabled"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsVision bool   `json:"supports_vision"`
}

// HandleProviderCatalog returns providers and their models for the setup
// wizard. Public (no JWT), but closed once setup is complete - post-setup the
// same data is served by the authenticated /admin/api/providers/catalog.
// Filters to kind='llm' so image providers don't pollute the chat-model picker.
func (h *AuthHandler) HandleProviderCatalog(w http.ResponseWriter, r *http.Request) {
	// 410 rather than the shared guard's 403: the wizard treats Gone as "setup
	// is finished, send me to sign-in" and reloads, which is the right response
	// to loading this screen on an already-claimed box.
	userCount, err := models.CountUsers(h.deps.DB.DB)
	if err != nil {
		h.deps.Logger.Error("setup providers: failed to count users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to check setup state")
		return
	}
	if userCount > 0 {
		writeError(w, http.StatusGone, "setup already completed")
		return
	}
	if !h.requireSetupToken(w, r) {
		return
	}

	providers, err := models.ListProvidersByKind(h.deps.DB.DB, "llm")
	if err != nil {
		h.deps.Logger.Error("setup providers: failed to list", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}

	result := make([]setupProviderResponse, 0, len(providers))
	for _, p := range providers {
		dbModels, err := models.ListModelsByProvider(h.deps.DB.DB, p.ID)
		if err != nil {
			h.deps.Logger.Error("setup providers: failed to list models", "provider", p.Name, "error", err)
			continue
		}

		modelList := make([]setupModelResponse, 0, len(dbModels))
		for _, m := range dbModels {
			modelList = append(modelList, setupModelResponse{
				ID:             m.ID,
				ModelID:        m.ModelID,
				DisplayName:    m.DisplayName,
				IsDefault:      m.IsDefault,
				IsEnabled:      m.IsEnabled,
				SupportsTools:  m.SupportsTools,
				SupportsVision: m.SupportsVision,
			})
		}

		result = append(result, setupProviderResponse{
			ID:             p.ID,
			Name:           p.Name,
			Vendor:         p.Vendor,
			Transport:      p.Transport,
			Plan:           p.Plan,
			IsEnabled:      p.IsEnabled,
			RequiresAPIKey: p.RequiresAPIKey,
			HasAPIKey:      p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != "",
			IsAvailable:    p.IsAvailable(),
			SignupURL:      p.SignupURL,
			Models:         modelList,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// HandleSetupSyncProvider is the setup wizard's "connect provider" step.
// Takes a provider id + api key, writes the encrypted key, then runs a
// synchronous ListModels sync so the wizard can immediately show the
// catalog. Public (no JWT) because it runs during first-time setup
// before any user exists; guarded by the same "no users yet" check
// HandleSetup uses to prevent post-setup abuse.
// requireFirstRun gates the unauthenticated setup endpoints: they exist only
// while the box is unclaimed. Once an admin account exists the whole surface
// moves behind JWT, so an attacker cannot use these to add a provider row to a
// running instance. Same guard HandleProviderCatalog and HandleSetupSyncProvider
// already use, factored out so a new setup endpoint cannot forget it.
func (h *AuthHandler) requireFirstRun(w http.ResponseWriter) bool {
	count, err := models.CountUsers(h.deps.DB.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count users")
		return false
	}
	if count > 0 {
		writeError(w, http.StatusForbidden, "setup already completed")
		return false
	}
	return true
}

func (h *AuthHandler) HandleSetupSyncProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireFirstRun(w) {
		return
	}
	if !h.requireSetupToken(w, r) {
		return
	}

	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider id")
	if !ok {
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	provider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if provider.Kind != "llm" {
		writeError(w, http.StatusBadRequest, "only LLM providers can be configured here")
		return
	}

	// Validate-then-persist: try the upstream catalog with the candidate key
	// BEFORE storing anything, or a typo'd key would sit encrypted on the row
	// and silently be reused on every retry while the wizard reports "Key
	// stored". On upstream reject: 400, providers row untouched (no key, no models).
	hasStoredKey := provider.APIKeyEncrypted != nil && *provider.APIKeyEncrypted != ""
	candidateKey := req.APIKey
	if candidateKey == "" {
		if !hasStoredKey {
			if provider.RequiresAPIKey {
				writeError(w, http.StatusBadRequest, "api_key is required for this provider")
				return
			}
		} else if plain, err := h.deps.Encryptor.Decrypt(*provider.APIKeyEncrypted); err == nil {
			candidateKey = plain
		}
	}
	live, err := llm.NewProvider(provider.Transport, provider.ProviderOptions(candidateKey, h.deps.LLMHTTPClient))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unsupported transport: "+err.Error())
		return
	}

	lister, ok := live.(llm.ModelLister)
	if !ok {
		// Non-listable backend (image/stock/automatic1111). We can't
		// validate the key by listing, so trust the user, but still
		// persist + register so the wizard can move on.
		if req.APIKey != "" {
			if err := h.persistSetupKey(providerID, req.APIKey); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		h.deps.LLMRegistry.Register(provider.Name, live)
		writeJSON(w, http.StatusOK, map[string]any{
			"provider_id": providerID,
			"synced":      false,
			"sync_error":  "this provider does not support model discovery, add a model manually after setup",
			"models":      []setupModelResponse{},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	discovered, listErr := lister.ListModels(ctx)
	if listErr != nil {
		h.deps.Logger.Warn("setup-sync: list models failed", "provider", provider.Name, "error", listErr)
		writeError(w, http.StatusBadRequest, friendlyListError(listErr))
		return
	}

	// Sync succeeded, now it's safe to persist the (new) key.
	if req.APIKey != "" {
		if err := h.persistSetupKey(providerID, req.APIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	h.deps.LLMRegistry.Register(provider.Name, live)

	// defaultEnabled=true: during setup the user picks from the freshly
	// synced list, so every model should be selectable. The provider-page
	// "Sync Models" button uses defaultEnabled=false because by then the
	// admin already has a working setup and a flood of new models would
	// just noise up the picker.
	stats, syncErr := syncLLMCatalog(h.deps, providerID, discovered, true)
	syncResp := map[string]any{
		"provider_id": providerID,
		"synced":      syncErr == nil,
	}
	if syncErr != nil {
		syncResp["sync_error"] = syncErr.Error()
	} else {
		syncResp["discovered"] = len(discovered)
		syncResp["added"] = stats.Added
		syncResp["updated"] = stats.Updated
	}

	dbModels, _ := models.ListModelsByProvider(h.deps.DB.DB, providerID)
	modelList := make([]setupModelResponse, 0, len(dbModels))
	for _, m := range dbModels {
		modelList = append(modelList, setupModelResponse{
			ID: m.ID, ModelID: m.ModelID, DisplayName: m.DisplayName,
			IsDefault: m.IsDefault, IsEnabled: m.IsEnabled,
			SupportsTools: m.SupportsTools, SupportsVision: m.SupportsVision,
		})
	}
	syncResp["models"] = modelList
	writeJSON(w, http.StatusOK, syncResp)
}

// persistSetupKey encrypts and writes the API key for a provider during
// the setup wizard's connect step. Used after ListModels has succeeded
// so we know the key works before storing it.
func (h *AuthHandler) persistSetupKey(providerID int, apiKey string) error {
	encrypted, err := h.deps.Encryptor.Encrypt(apiKey)
	if err != nil {
		h.deps.Logger.Error("setup-sync: encrypt failed", "error", err)
		return errors.New(msgcat.Mark("failed to encrypt api key"))
	}
	if _, err := h.deps.DB.ExecWrite(
		"UPDATE providers SET api_key_encrypted = ?, is_enabled = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		encrypted, providerID,
	); err != nil {
		h.deps.Logger.Error("setup-sync: write key failed", "error", err)
		return errors.New(msgcat.Mark("failed to save api key"))
	}
	return nil
}

// validateSetupCredentials mirrors the client-side rules in step-account.js
// so non-browser callers (curl, custom scripts) can't bypass the password
// requirements and seed a weak admin. Keep these rules in sync with the
// frontend, the two diverging is how easy it is to forget the server check.
func validateSetupCredentials(username, password string) error {
	u := strings.TrimSpace(username)
	if u == "" {
		return errors.New(msgcat.Mark("username is required"))
	}
	if utf8.RuneCountInString(u) > 64 {
		return errors.New(msgcat.Mark("username must be 64 characters or fewer"))
	}
	if u != username {
		return errors.New(msgcat.Mark("username cannot start or end with whitespace"))
	}
	return validatePasswordComplexity(password)
}

// validatePasswordComplexity enforces the shared password rules (length plus
// an uppercase letter, a digit, and a special character). It is the single
// source of truth used by setup and by admin user create/update so non-browser
// callers can't seed or downgrade an account to a weak password. The browser
// applies the same rules in core/password.js.
//
// The rules are about kinds of characters, not the Latin alphabet: length counts
// characters rather than bytes, "uppercase" is any capital letter or a letter from
// a script with no case (Japanese, Chinese, Arabic, Hindi), and "number" is any
// decimal digit. A person whose keyboard has no Latin capitals can still sign up.
func validatePasswordComplexity(password string) error {
	if utf8.RuneCountInString(password) < 8 {
		return errors.New(msgcat.Mark("password must be at least 8 characters"))
	}
	var hasUpper, hasDigit, hasSpecial bool
	for _, r := range password {
		if isCapitalOrCaseless(r) {
			hasUpper = true
		}
		if unicode.IsDigit(r) {
			hasDigit = true
		}
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			hasSpecial = true
		}
	}
	if !hasUpper {
		return errors.New(msgcat.Mark("password must contain an uppercase letter"))
	}
	if !hasDigit {
		return errors.New(msgcat.Mark("password must contain a number"))
	}
	if !hasSpecial {
		return errors.New(msgcat.Mark("password must contain a special character"))
	}
	return nil
}

// isCapitalOrCaseless reports a capital letter, or a letter from a script that has
// no upper and lower case at all.
func isCapitalOrCaseless(r rune) bool {
	return unicode.IsUpper(r) || (unicode.IsLetter(r) && !unicode.IsLower(r))
}

// friendlyListError converts a ListModels error into a message the setup
// wizard can show verbatim. The transport-level error strings contain
// HTTP status codes and raw response bodies; we lift out the common auth
// signals so the user gets actionable guidance instead of a stack trace.
func friendlyListError(err error) string {
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "401"), strings.Contains(low, "unauthorized"),
		strings.Contains(low, "invalid api key"), strings.Contains(low, "invalid_api_key"),
		strings.Contains(low, "authentication"):
		return msgcat.Mark("the provider rejected this API key, double-check it and try again")
	case strings.Contains(low, "403"), strings.Contains(low, "forbidden"):
		return msgcat.Mark("the provider refused the request; your key may not have permission for this account")
	case strings.Contains(low, "429"), strings.Contains(low, "rate limit"):
		return msgcat.Mark("the provider is rate-limiting; wait a moment and try again")
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"),
		strings.Contains(low, "dial tcp"), strings.Contains(low, "timeout"):
		return msgcat.Mark("could not reach the provider; check the base URL and your network")
	default:
		return fmt.Sprintf(msgcat.Mark("could not load models: %s"), s)
	}
}

// HandleSetup creates the initial admin user during first-run setup.
// Optionally updates an existing DB provider with the user's API key.
func (h *AuthHandler) HandleSetup(w http.ResponseWriter, r *http.Request) {
	if !h.requireSetupToken(w, r) {
		return
	}
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := validateSetupCredentials(req.Username, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Normalise the trimmed username back onto the request so downstream
	// inserts use the canonical form. Display name is allowed to keep
	// whatever spacing the user typed; only the login id is restricted.
	req.Username = strings.TrimSpace(req.Username)

	// Hash the password before acquiring the write lock (bcrypt is slow).
	hash, err := security.HashPassword(req.Password)
	if err != nil {
		h.deps.Logger.Error("setup: failed to hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Run all setup DB writes in a single transaction.
	// The user-count check is INSIDE the transaction to prevent a TOCTOU race
	// where concurrent requests both see count==0 and create duplicate admins.
	tx, err := h.deps.DB.BeginWriteTx()
	if err != nil {
		h.deps.Logger.Error("setup: failed to begin transaction", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		tx.Rollback()
		h.deps.Logger.Error("setup: failed to count users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to count users")
		return
	}
	if count > 0 {
		tx.Rollback()
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}

	user, err := models.CreateUserTx(tx, req.Username, hash, "admin", req.DisplayName)
	if err != nil {
		tx.Rollback()
		h.deps.Logger.Error("setup: failed to create user", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Optionally configure the selected provider with an API key.
	if req.ProviderID > 0 {
		provider, pErr := models.GetProviderByID(h.deps.DB.DB, req.ProviderID)
		if pErr != nil {
			h.deps.Logger.Warn("setup: provider not found", "id", req.ProviderID, "error", pErr)
		} else {
			// Update the API key only when the wizard sent one. Empty means
			// "reuse the stored key" (e.g. HandleSetupSyncProvider already
			// persisted it); overwriting with NULL would clear an
			// already-stored credential and leave the provider keyless.
			if req.APIKey != "" {
				encrypted, encErr := h.deps.Encryptor.Encrypt(req.APIKey)
				if encErr != nil {
					h.deps.Logger.Error("setup: failed to encrypt api key", "error", encErr)
				} else if _, err := tx.Exec(
					"UPDATE providers SET name = ?, api_key_encrypted = ?, base_url = ?, is_enabled = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
					provider.Name, encrypted, provider.BaseURL, provider.ID,
				); err != nil {
					h.deps.Logger.Error("setup: failed to update provider", "error", err)
				}
			} else if _, err := tx.Exec(
				"UPDATE providers SET name = ?, base_url = ?, is_enabled = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
				provider.Name, provider.BaseURL, provider.ID,
			); err != nil {
				h.deps.Logger.Error("setup: failed to update provider", "error", err)
			}

			// Clear is_default on ALL models first, then set the chosen one.
			if _, err := tx.Exec("UPDATE llm_models SET is_default = 0"); err != nil {
				tx.Rollback()
				h.deps.Logger.Error("setup: failed to clear model defaults", "error", err)
				writeError(w, http.StatusInternalServerError, "failed to configure provider")
				return
			}
			if req.ModelID != "" {
				if _, err := tx.Exec(
					"UPDATE llm_models SET is_default = 1 WHERE model_id = ? AND provider_id = ?",
					req.ModelID, provider.ID,
				); err != nil {
					tx.Rollback()
					h.deps.Logger.Error("setup: failed to set default model", "error", err)
					writeError(w, http.StatusInternalServerError, "failed to configure provider")
					return
				}
			} else {
				// No specific model chosen, default the first model of this provider.
				if _, err := tx.Exec(
					"UPDATE llm_models SET is_default = 1 WHERE id = (SELECT id FROM llm_models WHERE provider_id = ? ORDER BY id LIMIT 1)",
					provider.ID,
				); err != nil {
					tx.Rollback()
					h.deps.Logger.Error("setup: failed to set default model", "error", err)
					writeError(w, http.StatusInternalServerError, "failed to configure provider")
					return
				}
			}

			h.deps.Logger.Info("setup: configured provider",
				"provider", provider.Name,
				"id", strconv.Itoa(provider.ID),
			)

			// Register the provider in the live LLM registry (after commit).
			defer func() {
				if req.APIKey == "" {
					return
				}
				liveProvider, err := llm.NewProvider(provider.Transport, provider.ProviderOptions(req.APIKey, h.deps.LLMHTTPClient))
				if err != nil {
					h.deps.Logger.Warn("setup: failed to construct provider", "provider", provider.Name, "error", err)
					return
				}
				h.deps.LLMRegistry.Register(provider.Name, liveProvider)
				h.deps.Logger.Info("setup: registered provider in live registry", "provider", provider.Name)
			}()
		}
	}

	// Persist the chosen engine daily token budget as an admin setting in the
	// same atomic first-run write. Resolved live by the engine (env > this
	// setting > config/default), so it takes effect without a restart.
	if req.EngineDailyTokenBudget != nil && *req.EngineDailyTokenBudget >= 0 {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value, updated_at) VALUES ('engine_daily_token_budget', ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
			strconv.Itoa(*req.EngineDailyTokenBudget),
		); err != nil {
			tx.Rollback()
			h.deps.Logger.Error("setup: failed to save engine daily token budget", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to complete setup")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.deps.Logger.Error("setup: failed to commit transaction", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to complete setup")
		return
	}

	// Tell the running engine to re-resolve the budget (no restart needed).
	if req.EngineDailyTokenBudget != nil && h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventSettingsChanged, 0, nil))
	}

	// Generate a token for immediate login. A freshly created user is at
	// token_version 0.
	token, err := h.deps.JWTManager.Generate(security.TokenRequest{UserID: user.ID, Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion})
	if err != nil {
		h.deps.Logger.Error("setup: failed to generate token", "error", err)
		writeError(w, http.StatusInternalServerError, "user created but failed to generate token")
		return
	}

	h.deps.Logger.Info("setup completed", "username", user.Username)
	h.deps.JWTManager.SetAuthCookie(w, token)
	// The instance is claimed; the token has done its job.
	h.setupToken.Store(nil)

	writeJSON(w, http.StatusCreated, setupResponse{
		Token: token,
		User:  toUserView(user),
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// HandleChangePassword lets the signed-in user set their own password, which any
// role may do (the admin-only users page covers other accounts). The current
// password is required so a stolen token alone cannot take over the account, and
// wrong guesses count toward the same per-username lockout as login. The change
// bumps token_version, signing every other session out; the response carries a
// fresh token so this one stays signed in.
//
//	PUT /admin/api/auth/password
func (h *AuthHandler) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	claims, ok := r.Context().Value(security.ClaimsKey).(*security.Claims)
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "missing authorization")
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}

	user, err := models.GetUserByID(h.deps.DB.DB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}

	lockKey := strings.ToLower(strings.TrimSpace(user.Username))
	if locked, retryAfter := h.lockout.Locked(lockKey); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !security.CheckPassword(req.CurrentPassword, user.PasswordHash) {
		h.lockout.Fail(lockKey)
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	h.lockout.Reset(lockKey)

	if err := validatePasswordComplexity(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := security.HashPassword(req.NewPassword)
	if err != nil {
		h.deps.serverError(w, "failed to update password", err)
		return
	}
	if err := models.UpdateUserPassword(h.deps.DB.DB, user.ID, hash); err != nil {
		h.deps.serverError(w, "failed to update password", err)
		return
	}
	h.reissueSession(w, user.ID)
}

// HandleRevokeSessions signs the user out everywhere else: it bumps token_version,
// which invalidates every token issued so far (other browsers, a lost laptop, a
// leaked token), and returns a fresh token so this session carries on.
//
//	POST /admin/api/auth/revoke-sessions
func (h *AuthHandler) HandleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	claims, ok := r.Context().Value(security.ClaimsKey).(*security.Claims)
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "missing authorization")
		return
	}
	if err := models.BumpUserTokenVersion(h.deps.DB.DB, claims.UserID); err != nil {
		h.deps.serverError(w, "failed to revoke sessions", err)
		return
	}
	h.reissueSession(w, claims.UserID)
}

// reissueSession mints a token at the user's current token_version, sets the auth
// cookie and writes the same {token, user} body login does.
func (h *AuthHandler) reissueSession(w http.ResponseWriter, userID int) {
	user, err := models.GetUserByID(h.deps.DB.DB, userID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}
	token, err := h.deps.JWTManager.Generate(security.TokenRequest{UserID: user.ID, Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion})
	if err != nil {
		h.deps.serverError(w, "failed to generate token", err)
		return
	}
	h.deps.JWTManager.SetAuthCookie(w, token)
	writeJSON(w, http.StatusOK, loginResponse{Token: token, User: toUserView(user)})
}

// HandleSetupTokenCheck lets the wizard confirm the setup token as soon as it is
// entered, instead of failing a step later with a generic load error. 200 when the
// token is right (or none is required), 403 when it is wrong, 410 once an admin
// exists. POST, so the header-borne token is never part of a cacheable request.
//
//	POST /admin/api/setup/token
func (h *AuthHandler) HandleSetupTokenCheck(w http.ResponseWriter, r *http.Request) {
	n, err := models.CountUsers(h.deps.DB.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check setup state")
		return
	}
	if n > 0 {
		writeError(w, http.StatusGone, "setup already completed")
		return
	}
	if !h.requireSetupToken(w, r) {
		return
	}
	writeStatus(w, "ok")
}

// HandleLogout clears the auth cookie.
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	security.ClearAuthCookie(w)
	writeStatus(w, "ok")
}
