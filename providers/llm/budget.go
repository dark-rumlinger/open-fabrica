package llm

import (
	"database/sql"
	"strings"
)

// UnattendedSources are the of_llm_calls.source tags that count against a
// project's daily spend budget: the autonomous engine loop plus every other
// non-interactive path that can call the model on its own, event triggers,
// scheduled jobs, and server functions. Interactive "chat" is excluded (the
// operator is actively driving it). One list so the tally and every enforcement
// point agree on what "unattended spend" means.
var UnattendedSources = []string{"engine", "trigger", "job", "function"}

// PublicSources are the of_llm_calls.source tags end users of a generated app drive:
// the LLM proxy and hosted agent endpoints. They draw on their own daily token pool,
// so a busy public chatbot cannot pause the build, and a quiet build cannot leave the
// chatbot unbounded.
var PublicSources = []string{"proxy", "agent"}

// BillableSources is every source that bills the owner's key without the owner
// driving it. The dollar cap is one project-wide ceiling over all of them.
var BillableSources = append(append([]string{}, UnattendedSources...), PublicSources...)

// DefaultPublicDailyTokenBudget is the public pool's daily token budget when the
// project sets none.
const DefaultPublicDailyTokenBudget = 1_000_000

// TokensUsedTodayForSources returns the cost-weighted token spend since UTC
// midnight for the given of_llm_calls sources, for one project DB. Input and
// output count at face value; cached-prefix reads/writes are weighted by the
// MODEL's own cache price ratio (CacheCostFactorsFor) so heavy prompt-cache
// reuse, cheap in dollars but huge in raw tokens, is not misread as expensive.
// Grouping by (provider_name, model) rather than provider_type is what makes
// that per-model weight reachable: one transport serves dozens of vendors whose
// cache pricing has nothing in common. Pure cache replays log zero tokens and
// never count.
func TokensUsedTodayForSources(db *sql.DB, sources []string) (int64, error) {
	if db == nil || len(sources) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")
	args := make([]any, len(sources))
	for i, s := range sources {
		args[i] = s
	}
	rows, err := db.Query(
		`SELECT COALESCE(provider_type, ''), COALESCE(provider_name, ''), COALESCE(model, ''),
		        COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0), COALESCE(SUM(cache_creation_input_tokens), 0)
		   FROM of_llm_calls
		  WHERE source IN (`+placeholders+`) AND created_at >= date('now')
		  GROUP BY provider_type, provider_name, model`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total int64
	for rows.Next() {
		var providerType, providerName, model string
		var input, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&providerType, &providerName, &model, &input, &output, &cacheRead, &cacheCreate); err != nil {
			return 0, err
		}
		readF, createF := CacheCostFactorsFor(ModelKey{Provider: providerName, Model: model}, providerType)
		total += input + output + int64(float64(cacheRead)*readF) + int64(float64(cacheCreate)*createF)
	}
	return total, rows.Err()
}

// SpendLimits are one project's own daily limits. A nil field means "not set": the
// unattended token budget then falls back to the instance default, the public one to
// DefaultPublicDailyTokenBudget, and there is no dollar cap.
type SpendLimits struct {
	DailyTokenBudget       *int     `json:"daily_token_budget"`
	DailyPublicTokenBudget *int     `json:"daily_public_token_budget"`
	DailyUSDCap            *float64 `json:"daily_usd_cap"`
}

// LoadSpendLimits reads the project's limits. A missing row is "no overrides".
func LoadSpendLimits(db *sql.DB) SpendLimits {
	var l SpendLimits
	var tokens, publicTokens sql.NullInt64
	var usd sql.NullFloat64
	if err := db.QueryRow("SELECT daily_token_budget, daily_public_token_budget, daily_usd_cap FROM of_spend_limits WHERE id = 1").Scan(&tokens, &publicTokens, &usd); err != nil {
		return l
	}
	if tokens.Valid {
		n := int(tokens.Int64)
		l.DailyTokenBudget = &n
	}
	if publicTokens.Valid {
		n := int(publicTokens.Int64)
		l.DailyPublicTokenBudget = &n
	}
	if usd.Valid && usd.Float64 > 0 {
		f := usd.Float64
		l.DailyUSDCap = &f
	}
	return l
}

// SaveSpendLimits stores the limits; nil fields clear the override.
func SaveSpendLimits(db interface {
	Exec(query string, args ...any) (sql.Result, error)
}, l SpendLimits) error {
	var tokens, publicTokens any
	if l.DailyTokenBudget != nil {
		tokens = *l.DailyTokenBudget
	}
	if l.DailyPublicTokenBudget != nil {
		publicTokens = *l.DailyPublicTokenBudget
	}
	var usd any
	if l.DailyUSDCap != nil && *l.DailyUSDCap > 0 {
		usd = *l.DailyUSDCap
	}
	_, err := db.Exec(
		`INSERT INTO of_spend_limits (id, daily_token_budget, daily_public_token_budget, daily_usd_cap, updated_at) VALUES (1, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET daily_token_budget = excluded.daily_token_budget, daily_public_token_budget = excluded.daily_public_token_budget,
		   daily_usd_cap = excluded.daily_usd_cap, updated_at = CURRENT_TIMESTAMP`,
		tokens, publicTokens, usd)
	return err
}

// SpendUSDTodayForSources prices today's (UTC) calls for the given sources from
// each model's recorded prices. unpriced counts calls whose model has no price on
// record: they cannot be converted, so a dollar cap cannot see them and the
// caller should say so rather than present the figure as complete.
func SpendUSDTodayForSources(db *sql.DB, sources []string) (usd float64, unpriced int64, err error) {
	if db == nil || len(sources) == 0 {
		return 0, 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")
	args := make([]any, len(sources))
	for i, s := range sources {
		args[i] = s
	}
	rows, err := db.Query(
		`SELECT COALESCE(provider_name, ''), COALESCE(model, ''), COUNT(*),
		        COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0), COALESCE(SUM(cache_creation_input_tokens), 0)
		   FROM of_llm_calls
		  WHERE source IN (`+placeholders+`) AND created_at >= date('now')
		  GROUP BY provider_name, model`, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, model string
		var calls, input, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&provider, &model, &calls, &input, &output, &cacheRead, &cacheCreate); err != nil {
			return 0, 0, err
		}
		cost, ok := CallCostUSD(provider, model, int(input), int(output), int(cacheRead), int(cacheCreate))
		if !ok {
			if input+output+cacheRead+cacheCreate > 0 {
				unpriced += calls
			}
			continue
		}
		usd += cost
	}
	return usd, unpriced, rows.Err()
}

// SpendStatus is where a project stands against its daily limits.
type SpendStatus struct {
	Exceeded      bool
	Reason        string // owner-facing, set when Exceeded
	TokensUsed    int64
	TokenBudget   int     // effective budget; 0 = unlimited
	USDUsed       float64 // 0 unless a dollar cap is set
	USDCap        float64 // 0 = no cap
	UnpricedCalls int64
}

// CheckSpend measures the project's unattended spend (UnattendedSources) against its
// limits: the project's own token budget when set, else globalTokenBudget (0 =
// unlimited), plus the project-wide dollar cap. It fails open on any query error, so
// a transient DB hiccup never blocks a billable call.
func CheckSpend(db *sql.DB, globalTokenBudget int) SpendStatus {
	limits := LoadSpendLimits(db)
	budget := globalTokenBudget
	if limits.DailyTokenBudget != nil {
		budget = *limits.DailyTokenBudget
	}
	return checkPool(db, UnattendedSources, budget, limits)
}

// CheckPublicSpend measures the public pool (PublicSources: the LLM proxy and agent
// endpoints) against the project's public token budget, DefaultPublicDailyTokenBudget
// when unset (0 = unlimited), plus the project-wide dollar cap. Fails open like
// CheckSpend.
func CheckPublicSpend(db *sql.DB) SpendStatus {
	limits := LoadSpendLimits(db)
	budget := DefaultPublicDailyTokenBudget
	if limits.DailyPublicTokenBudget != nil {
		budget = *limits.DailyPublicTokenBudget
	}
	return checkPool(db, PublicSources, budget, limits)
}

// checkPool measures one pool's token spend against tokenBudget, and the dollar cap
// against every billable source, since the cap is one project-wide ceiling.
func checkPool(db *sql.DB, sources []string, tokenBudget int, limits SpendLimits) SpendStatus {
	st := SpendStatus{TokenBudget: tokenBudget}
	if limits.DailyUSDCap != nil {
		st.USDCap = *limits.DailyUSDCap
	}
	if st.TokenBudget > 0 {
		used, err := TokensUsedTodayForSources(db, sources)
		if err == nil {
			st.TokensUsed = used
			if used >= int64(st.TokenBudget) {
				st.Exceeded = true
				st.Reason = "the daily AI token limit"
			}
		}
	}
	if st.USDCap > 0 {
		usd, unpriced, err := SpendUSDTodayForSources(db, BillableSources)
		if err == nil {
			st.USDUsed, st.UnpricedCalls = usd, unpriced
			if usd >= st.USDCap && !st.Exceeded {
				st.Exceeded = true
				st.Reason = "the daily AI spending cap"
			}
		}
	}
	return st
}

// DailyBudgetExceeded reports whether a project has reached its daily unattended
// limits (see CheckSpend). globalBudget <= 0 with no project override and no
// dollar cap means unlimited and is never exceeded. It fails open: a query error
// returns exceeded=false so a transient DB hiccup never blocks a billable call,
// matching the engine's existing budget-check behavior. The token count is
// returned for logging.
func DailyBudgetExceeded(db *sql.DB, globalBudget int) (bool, int64, error) {
	st := CheckSpend(db, globalBudget)
	return st.Exceeded, st.TokensUsed, nil
}
