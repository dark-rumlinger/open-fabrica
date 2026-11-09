package engine

// Per-project daily spend ceiling for the autonomous engine loop, which calls
// the LLM unattended on the operator's own API key. Once reached, the next
// billable call fails with ErrBudgetExceeded and the worker pauses with an
// owner-facing message. Zero = unlimited.

import (
	"errors"
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/internal/msgcat"
	"openfabrica/providers/llm"
)

// ErrBudgetExceeded is returned by the LLM caller when a billable call would
// push the project past its configured daily engine token budget.
var ErrBudgetExceeded = errors.New("engine daily token budget exceeded")

// isBudgetError reports whether err is (or wraps) ErrBudgetExceeded.
func isBudgetError(err error) bool { return errors.Is(err, ErrBudgetExceeded) }

// engineTokensUsedToday returns today's (UTC) cost-weighted token spend across
// ALL unattended sources (llm.UnattendedSources): the daily budget is one shared
// pool. The weighting lives in llm.TokensUsedTodayForSources so the tally and
// every other enforcement point can never drift.
func engineTokensUsedToday(pdb *db.ProjectDB) (int64, error) {
	return llm.TokensUsedTodayForSources(pdb.Reader(), llm.UnattendedSources)
}

// budgetPauseMessage returns an owner-facing pause message when err is (or
// wraps) ErrBudgetExceeded, plus true. Returns ("", false) otherwise.
func budgetPauseMessage(err error) (string, bool) {
	if !isBudgetError(err) {
		return "", false
	}
	if strings.Contains(err.Error(), "spending cap") {
		return msgcat.Mark("I've reached the daily AI spending cap set for this project, so I've paused for now. I'll pick back up after midnight (UTC), or you can raise the project's spending cap in its settings to continue sooner."), true
	}
	return msgcat.Mark("I've reached the daily AI spending limit set for this project, so I've paused for now. I'll pick back up after midnight (UTC), or you can raise the project's daily limit in its settings (or OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET for the instance default) to continue sooner."), true
}

// budgetExceeded returns a wrapped ErrBudgetExceeded carrying the usage numbers
// for the logs. The reason text tells the two limits apart for the pause message.
func budgetExceeded(st llm.SpendStatus) error {
	if st.USDCap > 0 && strings.Contains(st.Reason, "spending cap") {
		return fmt.Errorf("%w: %s reached, spent $%.2f of $%.2f today", ErrBudgetExceeded, st.Reason, st.USDUsed, st.USDCap)
	}
	return fmt.Errorf("%w: %s reached, used %d of %d tokens today", ErrBudgetExceeded, st.Reason, st.TokensUsed, st.TokenBudget)
}
