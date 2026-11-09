package admin

import (
	"database/sql"
	"net/http"

	"openfabrica/providers/llm"
)

// ProjectSpendHandler reads and sets one project's own daily AI spend limits: a
// token budget for the unattended pool (the build loop, triggers, jobs, server
// functions) that overrides the instance default, a token budget for the public pool
// (the LLM proxy and agent endpoints end users call), and one dollar cap over both.
type ProjectSpendHandler struct {
	deps *Deps
}

// instanceTokenBudget is the live instance-wide default the project's unattended
// budget overrides (the resolver main wires for every unattended path).
func (h *ProjectSpendHandler) instanceTokenBudget() int {
	if h.deps.DailyTokenBudgetFn != nil {
		return h.deps.DailyTokenBudgetFn()
	}
	return 0
}

// Get returns the limits and where today's spend stands against them.
func (h *ProjectSpendHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, h.status(projectDB.Reader()))
}

func (h *ProjectSpendHandler) status(reader *sql.DB) map[string]any {
	limits := llm.LoadSpendLimits(reader)
	instance := h.instanceTokenBudget()
	st := llm.CheckSpend(reader, instance)
	pub := llm.CheckPublicSpend(reader)
	usd, unpriced, _ := llm.SpendUSDTodayForSources(reader, llm.BillableSources)
	tokens, _ := llm.TokensUsedTodayForSources(reader, llm.UnattendedSources)
	publicTokens, _ := llm.TokensUsedTodayForSources(reader, llm.PublicSources)
	return map[string]any{
		"daily_token_budget":            limits.DailyTokenBudget,
		"daily_public_token_budget":     limits.DailyPublicTokenBudget,
		"daily_usd_cap":                 limits.DailyUSDCap,
		"instance_token_budget":         instance,
		"default_public_token_budget":   llm.DefaultPublicDailyTokenBudget,
		"effective_token_budget":        st.TokenBudget,
		"effective_public_token_budget": pub.TokenBudget,
		"tokens_used_today":             tokens,
		"public_tokens_used_today":      publicTokens,
		"usd_used_today":                usd,
		"unpriced_calls_today":          unpriced,
		"paused_by_limit":               st.Exceeded,
		"public_paused_by_limit":        pub.Exceeded,
	}
}

// Update sets the limits. A null (or absent) field clears that override.
func (h *ProjectSpendHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	var req struct {
		DailyTokenBudget       *int     `json:"daily_token_budget"`
		DailyPublicTokenBudget *int     `json:"daily_public_token_budget"`
		DailyUSDCap            *float64 `json:"daily_usd_cap"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DailyTokenBudget != nil && *req.DailyTokenBudget < 0 {
		writeError(w, http.StatusBadRequest, "daily_token_budget must be 0 (unlimited) or more")
		return
	}
	if req.DailyPublicTokenBudget != nil && *req.DailyPublicTokenBudget < 0 {
		writeError(w, http.StatusBadRequest, "daily_public_token_budget must be 0 (unlimited) or more")
		return
	}
	if req.DailyUSDCap != nil && (*req.DailyUSDCap < 0 || *req.DailyUSDCap > 1_000_000) {
		writeError(w, http.StatusBadRequest, "daily_usd_cap must be between 0 and 1000000")
		return
	}
	if err := llm.SaveSpendLimits(projectDB.ToolHandle(r.Context()), llm.SpendLimits{
		DailyTokenBudget: req.DailyTokenBudget, DailyPublicTokenBudget: req.DailyPublicTokenBudget, DailyUSDCap: req.DailyUSDCap,
	}); err != nil {
		h.deps.serverError(w, "failed to save spend limits", err)
		return
	}
	writeJSON(w, http.StatusOK, h.status(projectDB.Reader()))
}
