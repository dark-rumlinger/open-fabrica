// Package config loads and validates server configuration from the config
// file, environment overrides, and safe defaults.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AdminPort int `json:"admin_port"`
	// AdminBindAddr is the network interface the admin/control-plane listener
	// binds to. Defaults to 127.0.0.1 (loopback only). Set to "0.0.0.0" (and
	// OPENFABRICA_ADMIN_EXPOSE=1) to deliberately expose the admin panel to a
	// network - only do this behind a firewall or reverse proxy you control.
	AdminBindAddr string `json:"admin_bind_addr"`
	PublicPort    int    `json:"public_port"`
	DataDir       string `json:"data_dir"`
	DBPath        string `json:"db_path"`
	JWTSecret     string `json:"jwt_secret"`
	EncryptionKey string `json:"encryption_key"`
	LogLevel      string `json:"log_level"`
	// LogFile, when set, also writes the log to this file, rotated at 20 MB with
	// five old files kept. A relative path is inside the data directory. Empty
	// logs to the console only.
	LogFile  string `json:"log_file"`
	SeedPath string `json:"seed_path"`
	// TLSDisabled is a kill-switch for hosts where ports 80/443 are owned by
	// something else (e.g. nginx). When false (default), TLS auto-starts the
	// first time a project gains a Domain - no restart needed.
	TLSDisabled bool `json:"tls_disabled"`
	// TLSStaging routes ACME requests to Let's Encrypt's staging CA.
	// Browsers won't trust the certs, but it sidesteps LE's production
	// rate limit (50/week/domain) while iterating on a new install.
	TLSStaging  bool     `json:"tls_staging"`
	CORSOrigins []string `json:"cors_origins"`
	// TrustedProxies lists IPs or CIDRs of reverse proxies in front of the public
	// server whose X-Forwarded-For header is believed (loopback always is). Set it
	// when a proxy that is not on this host (a container network, a load balancer)
	// fronts the server, so rate limits and IP bans see the real visitor.
	TrustedProxies []string `json:"trusted_proxies"`
	RateLimitRate  float64  `json:"rate_limit_rate"`
	RateLimitBurst int      `json:"rate_limit_burst"`

	// LLM HTTP client timeout in seconds. Zero means use default (900s / 15 minutes).
	LLMTimeoutSec int `json:"llm_timeout_sec"`

	// EngineDailyTokenBudget caps total engine token spend per UTC day per
	// project, cost-weighted (cached-prefix reads discounted, a pure replay counts
	// zero). Zero = unlimited. A soft cap checked before each billable call; it
	// guards the unattended build loop from running up an unexpected bill, and the
	// worker pauses with an owner-facing message when a project reaches it.
	EngineDailyTokenBudget int `json:"engine_daily_token_budget"`

	// Engine per-stage wall-clock timeouts in minutes. Zero falls back to the
	// engine's built-in defaults (expand 2m, plan 30m, build 90m). Raise these
	// when a slow provider makes a legitimate build exceed the default rather
	// than rebuilding the binary.
	EngineStageExpandTimeoutMin int `json:"engine_stage_expand_timeout_min"`
	EngineStagePlanTimeoutMin   int `json:"engine_stage_plan_timeout_min"`
	EngineStageBuildTimeoutMin  int `json:"engine_stage_build_timeout_min"`

	// AutoOpenBrowser opens the user's default browser to the admin URL on
	// startup. Defaults to true; honor OPENFABRICA_NO_BROWSER as an override.
	AutoOpenBrowser bool `json:"auto_open_browser"`

	// PublicCSP is the Content-Security-Policy header applied to HTML
	// responses on the public project router. Empty disables the header.
	// Defaults to DefaultPublicCSP; set to "" to opt out.
	PublicCSP string `json:"public_csp"`
}

// DefaultConfigPath is the config file read when no path is supplied on the
// command line. A missing file is not an error: built-in defaults plus
// environment overrides still produce a valid configuration.
const DefaultConfigPath = "config.json"

// LoadFrom reads configuration from the file at path (a missing file is not an
// error), applies environment overrides, expands CSP presets, and derives the
// data directory, database path, and security keys.
func LoadFrom(path string) (*Config, error) {
	cfg := defaults()

	// A missing config file is fine; one that exists but cannot be read (wrong
	// permissions, a directory) must not silently fall back to defaults.
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Environment variable overrides
	applyEnvOverrides(cfg)

	// Expand a CSP preset keyword (e.g. OPENFABRICA_PUBLIC_CSP=hardened) into its
	// full policy so every downstream consumer sees the real header value.
	cfg.PublicCSP = ExpandCSPPreset(cfg.PublicCSP)

	// Resolve the data directory once, against the working directory the process
	// started in. The default is relative ("./data"), so launching from another
	// folder would otherwise quietly start a brand-new instance with new keys.
	if abs, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = abs
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0750); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	// Set DB path relative to data dir if not explicitly set
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "openfabrica.db")
	}

	// Auto-generate security keys on first run
	securityDir := filepath.Join(cfg.DataDir, ".security")
	if err := os.MkdirAll(securityDir, 0700); err != nil {
		return nil, fmt.Errorf("creating security dir: %w", err)
	}

	// dataDirIsPopulated is "this looks like an existing install, not first
	// run". Used to refuse silent key regeneration: losing jwt.key on a
	// populated data dir would otherwise invalidate every session with no
	// warning and (for encryption.key) make every encrypted column unreadable.
	dataDirIsPopulated := false
	if _, err := os.Stat(cfg.DBPath); err == nil {
		dataDirIsPopulated = true
	}
	allowKeyRegen := os.Getenv("OPENFABRICA_ALLOW_KEY_REGEN") == "1"

	if cfg.JWTSecret == "" {
		secret, err := loadOrGenerateKey(filepath.Join(securityDir, "jwt.key"), 32, dataDirIsPopulated && !allowKeyRegen)
		if err != nil {
			return nil, fmt.Errorf("jwt key: %w", err)
		}
		cfg.JWTSecret = secret
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT secret must be at least 32 characters (got %d)", len(cfg.JWTSecret))
	}

	if cfg.EncryptionKey == "" {
		key, err := loadOrGenerateKey(filepath.Join(securityDir, "encryption.key"), 32, dataDirIsPopulated && !allowKeyRegen)
		if err != nil {
			return nil, fmt.Errorf("encryption key: %w", err)
		}
		cfg.EncryptionKey = key
	}
	if len(cfg.EncryptionKey) != 64 {
		return nil, fmt.Errorf("encryption key must be exactly 64 hex characters (32 bytes), got %d", len(cfg.EncryptionKey))
	}

	// Validate port ranges.
	if cfg.AdminPort < 1 || cfg.AdminPort > 65535 {
		return nil, fmt.Errorf("admin_port must be 1-65535, got %d", cfg.AdminPort)
	}
	if cfg.PublicPort < 1 || cfg.PublicPort > 65535 {
		return nil, fmt.Errorf("public_port must be 1-65535, got %d", cfg.PublicPort)
	}

	// Normalise an empty bind address back to loopback so "" can never be read
	// as ":port" (all interfaces) downstream.
	if strings.TrimSpace(cfg.AdminBindAddr) == "" {
		cfg.AdminBindAddr = DefaultAdminBindAddr
	}
	// Fail closed when the admin panel would listen on a routable address without
	// an explicit acknowledgement: the control plane can read every project's data
	// and decrypt stored secrets, so exposing it is a deliberate act, not a default.
	if cfg.AdminExposed() && os.Getenv("OPENFABRICA_ADMIN_EXPOSE") != "1" {
		return nil, fmt.Errorf(
			"admin_bind_addr %q would expose the admin panel beyond this machine; "+
				"the admin panel can read all project data and decrypt stored secrets. "+
				"Keep it on 127.0.0.1 (default) and reach it via SSH tunnel, or set "+
				"OPENFABRICA_ADMIN_EXPOSE=1 to confirm you intend to expose it behind "+
				"your own firewall/reverse proxy", cfg.AdminBindAddr)
	}
	if cfg.AdminExposed() {
		slog.Warn("admin panel is bound to a routable address and reachable beyond this machine - "+
			"ensure it sits behind a firewall or reverse proxy with its own authentication",
			"admin_bind_addr", cfg.AdminBindAddr, "admin_port", cfg.AdminPort)
	}

	return cfg, nil
}

// AdminExposed reports whether the configured admin bind address is reachable
// from other machines (i.e. not a loopback interface).
func (c *Config) AdminExposed() bool {
	return !isLoopbackHost(c.AdminBindAddr)
}

// isLoopbackHost reports whether host refers only to the local machine.
// "0.0.0.0" and "::" are explicitly all-interfaces, so they are not loopback.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func defaults() *Config {
	return &Config{
		AdminPort:              DefaultAdminPort,
		AdminBindAddr:          DefaultAdminBindAddr,
		PublicPort:             DefaultPublicPort,
		DataDir:                DefaultDataDir,
		LogLevel:               DefaultLogLevel,
		SeedPath:               DefaultSeedPath,
		TLSDisabled:            false,
		TLSStaging:             false,
		AutoOpenBrowser:        true,
		RateLimitRate:          DefaultRateLimitRate,
		RateLimitBurst:         DefaultRateLimitBurst,
		EngineDailyTokenBudget: DefaultEngineDailyTokenBudget,
		PublicCSP:              DefaultPublicCSP,
	}
}

func applyEnvOverrides(cfg *Config) {
	if p, ok := envInt("OPENFABRICA_ADMIN_PORT", 1); ok {
		cfg.AdminPort = p
	}
	if v := os.Getenv("OPENFABRICA_ADMIN_BIND"); v != "" {
		cfg.AdminBindAddr = v
	}
	if p, ok := envInt("OPENFABRICA_PUBLIC_PORT", 1); ok {
		cfg.PublicPort = p
	}
	if v := os.Getenv("OPENFABRICA_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("OPENFABRICA_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("OPENFABRICA_JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv("OPENFABRICA_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
	}
	if v := os.Getenv("OPENFABRICA_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("OPENFABRICA_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = splitList(v)
	}
	if v := os.Getenv("OPENFABRICA_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("OPENFABRICA_SEED_PATH"); v != "" {
		cfg.SeedPath = v
	}
	if envTrue(os.Getenv("OPENFABRICA_TLS_DISABLED")) {
		cfg.TLSDisabled = true
	}
	if envTrue(os.Getenv("OPENFABRICA_TLS_STAGING")) {
		cfg.TLSStaging = true
	}
	if v := os.Getenv("OPENFABRICA_RATE_LIMIT_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.RateLimitRate = f
		} else {
			slog.Warn("ignoring invalid environment value", "name", "OPENFABRICA_RATE_LIMIT_RATE", "value", v, "want", "a number above 0")
		}
	}
	if n, ok := envInt("OPENFABRICA_RATE_LIMIT_BURST", 1); ok {
		cfg.RateLimitBurst = n
	}
	if n, ok := envInt("OPENFABRICA_LLM_TIMEOUT", 1); ok {
		cfg.LLMTimeoutSec = n
	}
	if n, ok := envInt("OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET", 0); ok {
		cfg.EngineDailyTokenBudget = n
	}
	if n, ok := envInt("OPENFABRICA_ENGINE_STAGE_EXPAND_TIMEOUT_MIN", 1); ok {
		cfg.EngineStageExpandTimeoutMin = n
	}
	if n, ok := envInt("OPENFABRICA_ENGINE_STAGE_PLAN_TIMEOUT_MIN", 1); ok {
		cfg.EngineStagePlanTimeoutMin = n
	}
	if n, ok := envInt("OPENFABRICA_ENGINE_STAGE_BUILD_TIMEOUT_MIN", 1); ok {
		cfg.EngineStageBuildTimeoutMin = n
	}
	if v, ok := os.LookupEnv("OPENFABRICA_PUBLIC_CSP"); ok {
		// Operators serving untrusted/multi-user apps can harden the public CSP
		// here; the literal token {nonce} in a script-src opts into per-request
		// nonces. Empty string opts out of the header; unset keeps the permissive default.
		cfg.PublicCSP = v
	}
	if envTrue(os.Getenv("OPENFABRICA_NO_BROWSER")) {
		cfg.AutoOpenBrowser = false
	}
}

// LLMTimeout returns the configured LLM HTTP client timeout as a time.Duration.
func (c *Config) LLMTimeout() time.Duration {
	if c.LLMTimeoutSec > 0 {
		return time.Duration(c.LLMTimeoutSec) * time.Second
	}
	return time.Duration(DefaultLLMTimeoutSec) * time.Second
}

func loadOrGenerateKey(path string, size int, refuseRegen bool) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	// Refuse to mint a fresh key on top of an existing install: generating here
	// would invalidate sessions (jwt.key) or render every encrypted column
	// unreadable (encryption.key). Restore from backup, or set
	// OPENFABRICA_ALLOW_KEY_REGEN=1 to acknowledge the loss.
	if refuseRegen {
		return "", fmt.Errorf(
			"%s is missing but the data dir already contains a database; "+
				"restore the key from backup or set OPENFABRICA_ALLOW_KEY_REGEN=1 "+
				"to generate a new one (this will invalidate sessions and any "+
				"data encrypted with the previous key)", path)
	}

	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}

	hexKey := hex.EncodeToString(key)
	if err := os.WriteFile(path, []byte(hexKey), 0600); err != nil {
		return "", err
	}

	slog.Info("generated security key", "path", path)
	// Losing the at-rest encryption key makes every stored secret (provider API
	// keys, OAuth tokens, payment credentials, TOTP seeds, and any AI-defined
	// encrypted column) permanently unrecoverable - there is no rotation/recovery
	// path. Make the backup obligation loud on the very first run.
	if filepath.Base(path) == "encryption.key" {
		slog.Warn("a new at-rest encryption key was generated - BACK IT UP NOW. "+
			"Back up this file TOGETHER WITH your data dir; without it, all stored "+
			"secrets become permanently unreadable.", "path", path)
	}
	return hexKey, nil
}

// splitList turns a comma-separated setting into its trimmed, non-empty items.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envTrue reads a boolean environment value: 1, true, yes or on, in any case.
// Anything else, including "0" and "false", is off.
func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envInt reads an integer environment value of at least min. An unset variable
// reports false quietly; a set but unusable one is logged so a typo in a port or
// timeout does not silently leave the default in place.
func envInt(name string, min int) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < min {
		slog.Warn("ignoring invalid environment value", "name", name, "value", v, "want", fmt.Sprintf("an integer of at least %d", min))
		return 0, false
	}
	return n, true
}
