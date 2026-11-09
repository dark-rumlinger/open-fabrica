package actionexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/internal/projectmodel"
	"openfabrica/internal/replaymodel"
	"openfabrica/providers/llm"
	"openfabrica/security"
)

// CompleteDeps are the process-wide pieces a one-shot completion needs. Every field
// but GlobalDB is optional: a nil HTTPClient uses the transport default, a nil
// BudgetFn skips the daily budget, and a nil Call runs the provider directly.
type CompleteDeps struct {
	GlobalDB   *sql.DB
	Encryptor  *security.Encryptor
	HTTPClient *http.Client
	BudgetFn   func() int
	Call       llm.CallFunc
}

// CompleteRequest is one prompt for the project's model.
type CompleteRequest struct {
	Prompt    string
	System    string
	MaxTokens int
	Timeout   time.Duration
}

// Complete runs one unattended completion (a trigger's llm_call, a job's llm_prompt,
// a server function's ctx.llm) the same way for every caller: the daily budget gate,
// the project's own model, the shared HTTP client, a row in of_llm_calls under
// source, and the engine's retry policy. projectDB is the project's writer.
func Complete(ctx context.Context, d CompleteDeps, projectID int, projectDB *sql.DB, source string, req CompleteRequest) (string, error) {
	if d.GlobalDB == nil {
		return "", fmt.Errorf("LLM not wired")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return "", fmt.Errorf("'prompt' is required")
	}
	// Fails open on a DB error, so a transient hiccup never blocks a legitimate call.
	if d.BudgetFn != nil && projectDB != nil {
		if st := llm.CheckSpend(projectDB, d.BudgetFn()); st.Exceeded {
			return "", fmt.Errorf("%s reached (%d tokens today); skipping until midnight UTC", st.Reason, st.TokensUsed)
		}
	}
	var providerID int
	provider, modelID, err := replaymodel.Resolve(projectID, func() (llm.Provider, string, error) {
		res, err := projectmodel.Resolve(d.GlobalDB, d.Encryptor, d.HTTPClient, projectID)
		providerID = res.ProviderID
		return res.Provider, res.Model, err
	})
	if err != nil {
		return "", err
	}
	if projectDB != nil {
		lg := llm.NewDBLLMLogger(projectDB)
		defer lg.Close()
		provider = llm.NewLoggedProvider(provider, lg, source, "", nil, nil)
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	resp, err := d.Call.Call(ctx, providerID, provider, llm.CompletionRequest{
		Model:     modelID,
		System:    req.System,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: req.Prompt}},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
