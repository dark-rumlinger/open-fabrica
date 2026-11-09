package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/providercfg"
	"openfabrica/internal/safego"
	"openfabrica/providers/llm"
)

// Capability probes settle what a model listing leaves unknown: whether a model
// reasons (which decides whether its edit dialog offers the per-stage thinking
// switches) and whether it calls tools (a model that cannot, cannot build).
// Anthropic, OpenRouter and Gemini listings report both; the plain OpenAI envelope
// (Z.ai, DeepSeek, Kimi, local runtimes) lists ids only.
//
// Probing is lazy. A sync never probes, so a provider listing 60 models syncs at
// once. A model is probed when it starts to matter: when the admin enables it, opens
// its edit dialog, or a project picks it, and on request from the dialog or the
// provider's "check capabilities" button. A verdict is kept on the row; a model is
// not probed again for a capability it has one for unless the admin asks.

const (
	// probeWorkers is how many models a provider-wide check probes at once.
	probeWorkers = 4
	// probeCallTimeout bounds one probe call: a local runtime loading a large model
	// into memory is the slow case.
	probeCallTimeout = 20 * time.Second
	// probeModelBudget bounds every probe of one model.
	probeModelBudget = 45 * time.Second
	// probeProviderBudget bounds a provider-wide check.
	probeProviderBudget = 3 * time.Minute
)

// listingReportsCaps reports whether the provider's model listing states reasoning
// and tool support itself, so its models are never probed.
func listingReportsCaps(p *models.LLMProvider) bool {
	if p.Transport == "anthropic" {
		return true
	}
	switch providercfg.Parse(p.Config).EffectiveShape() {
	case providercfg.ShapeOpenRouter, providercfg.ShapeGemini:
		return true
	}
	return false
}

// probeCapsFor lists the capabilities worth probing on a model: those its listing
// cannot report and the admin has not set by hand, and, unless force, those without
// a stored verdict.
func probeCapsFor(p *models.LLMProvider, m *models.LLMModel, force bool) []string {
	if p.Kind != "llm" || listingReportsCaps(p) {
		return nil
	}
	ov := m.Overrides()
	var out []string
	for _, c := range models.ProbeCaps {
		if (c == "reasoning" && ov.SupportsReasoning != nil) || (c == "tools" && ov.SupportsTools != nil) {
			continue
		}
		if _, probed := m.ProbeVerdict(c); probed && !force {
			continue
		}
		out = append(out, c)
	}
	return out
}

// applyProbeVerdicts carries stored verdicts onto a fresh listing, so a sync whose
// listing cannot report a capability does not reset what a probe settled.
func applyProbeVerdicts(d *Deps, providerID int, discovered []llm.ModelInfo) error {
	existing, err := models.ListModelsByProvider(d.DB.DB, providerID)
	if err != nil {
		return err
	}
	byID := make(map[string]*models.LLMModel, len(existing))
	for i := range existing {
		byID[existing[i].ModelID] = &existing[i]
	}
	for i := range discovered {
		row, ok := byID[discovered[i].ID]
		if !ok {
			continue
		}
		if has, probed := row.ProbeVerdict("reasoning"); probed {
			discovered[i].SupportsReasoning = has
		}
		if has, probed := row.ProbeVerdict("tools"); probed {
			discovered[i].SupportsTools = has
		}
	}
	return nil
}

// syncLLMCatalog writes a discovered LLM catalog, keeping probe verdicts. It never
// probes: that waits until a model matters.
func syncLLMCatalog(d *Deps, providerID int, discovered []llm.ModelInfo, defaultEnabled bool) (models.SyncModelStats, error) {
	if err := applyProbeVerdicts(d, providerID, discovered); err != nil {
		return models.SyncModelStats{}, err
	}
	return models.SyncProviderModels(d.DB.DB, providerID, llm.SyncedRows(discovered), defaultEnabled)
}

// probeModelCaps runs the given probes on one model and records each verdict. It
// returns the first rate-limit error, so a batch can stop; other failures record
// nothing and the model is asked again next time.
func probeModelCaps(ctx context.Context, d *Deps, live llm.Provider, m *models.LLMModel, caps []string) error {
	for _, c := range caps {
		pctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
		var verdict llm.ProbeVerdict
		var err error
		switch c {
		case "reasoning":
			verdict, err = llm.ProbeReasoning(pctx, live, m.ModelID)
		case "tools":
			verdict, err = llm.ProbeTools(pctx, live, m.ModelID)
		}
		cancel()
		if verdict == llm.ProbeUnknown {
			d.Logger.Info("capability probe could not tell", "model", m.ModelID, "capability", c, "error", err)
			if llm.IsRateLimited(err) {
				return err
			}
			continue
		}
		// Read the config back: an edit may have landed since the model was loaded.
		cur, gerr := models.GetModelByID(d.DB.DB, m.ID)
		if gerr != nil {
			return gerr
		}
		if werr := models.SetProbeVerdict(d.DB.DB, m.ID, cur.Config, c, verdict == llm.ProbeYes); werr != nil {
			d.Logger.Warn("capability probe write failed", "model", m.ModelID, "error", werr)
			continue
		}
		d.Logger.Info("capability probe", "model", m.ModelID, "capability", c, "has", verdict == llm.ProbeYes)
	}
	return nil
}

// liveLLMProvider returns the registered live provider for p, or builds one from the
// stored credentials and registers it.
func liveLLMProvider(d *Deps, p models.LLMProvider) (llm.Provider, error) {
	if live, err := d.LLMRegistry.Get(p.Name); err == nil {
		return live, nil
	}
	var apiKey string
	if p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != "" {
		plain, err := d.Encryptor.Decrypt(*p.APIKeyEncrypted)
		if err != nil {
			return nil, err
		}
		apiKey = plain
	}
	live, err := llm.NewProvider(p.Transport, p.ProviderOptions(apiKey, d.LLMHTTPClient))
	if err != nil {
		return nil, err
	}
	d.LLMRegistry.Register(p.Name, live)
	return live, nil
}

// probesInFlight holds the model ids being probed in the background, so enabling a
// model twice in a row does not probe it twice.
var probesInFlight sync.Map

// probeModelInBackground probes a model's unsettled capabilities without holding up
// the caller: the trigger for "this model now matters" (enabled, picked by a project).
func probeModelInBackground(d *Deps, modelPK int) {
	if _, busy := probesInFlight.LoadOrStore(modelPK, true); busy {
		return
	}
	safego.Go("capability-probe", func() {
		defer probesInFlight.Delete(modelPK)
		m, err := models.GetModelByID(d.DB.DB, modelPK)
		if err != nil {
			return
		}
		p, err := models.GetProviderByID(d.DB.DB, m.ProviderID)
		if err != nil {
			return
		}
		caps := probeCapsFor(p, m, false)
		if len(caps) == 0 {
			return
		}
		live, err := liveLLMProvider(d, *p)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeModelBudget)
		defer cancel()
		_ = probeModelCaps(ctx, d, live, m, caps)
	})
}

// ProbeModel probes one model now and returns it. The edit dialog calls it on open
// for a model with unsettled capabilities, and with force=1 from its "check again".
func (h *ProvidersHandler) ProbeModel(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}
	modelPK, ok := requireIntParam(w, r, "modelID", "invalid model ID")
	if !ok {
		return
	}
	p, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	m, err := models.GetModelByID(h.deps.DB.DB, modelPK)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}
	if m.ProviderID != providerID {
		writeError(w, http.StatusBadRequest, "model does not belong to this provider")
		return
	}
	if caps := probeCapsFor(p, m, r.URL.Query().Get("force") == "1"); len(caps) > 0 {
		live, lerr := liveLLMProvider(h.deps, *p)
		if lerr != nil {
			h.deps.serverError(w, "failed to update model", lerr)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		ctx, cancel := context.WithTimeout(r.Context(), probeModelBudget)
		defer cancel()
		_ = probeModelCaps(ctx, h.deps, live, m, caps)
	}
	fresh, err := models.GetModelByID(h.deps.DB.DB, modelPK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload model")
		return
	}
	resp := toModelResponse(*fresh)
	resp.ProbePending = probeCapsFor(p, fresh, false)
	writeJSON(w, http.StatusOK, resp)
}

// ProbeProviderModels re-checks every enabled model of a provider, a few at a time,
// and stops early when the provider starts rate limiting.
func (h *ProvidersHandler) ProbeProviderModels(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}
	p, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	rows, err := models.ListModelsByProvider(h.deps.DB.DB, providerID)
	if err != nil {
		h.deps.serverError(w, "failed to list models", err)
		return
	}
	var todo []models.LLMModel
	for _, m := range rows {
		if m.IsEnabled && len(probeCapsFor(p, &m, true)) > 0 {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"checked": 0, "total": 0, "rate_limited": false})
		return
	}
	live, err := liveLLMProvider(h.deps, *p)
	if err != nil {
		h.deps.serverError(w, "failed to update model", err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(r.Context(), probeProviderBudget)
	defer cancel()

	var (
		mu          sync.Mutex
		checked     int
		rateLimited bool
		wg          sync.WaitGroup
	)
	jobs := make(chan models.LLMModel)
	for range min(probeWorkers, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			safego.Run("capability-probe-worker", func() {
				for m := range jobs {
					err := probeModelCaps(ctx, h.deps, live, &m, probeCapsFor(p, &m, true))
					mu.Lock()
					checked++
					if llm.IsRateLimited(err) && !rateLimited {
						rateLimited = true
						cancel()
					}
					mu.Unlock()
				}
			})
		}()
	}
	for _, m := range todo {
		if ctx.Err() != nil {
			break
		}
		jobs <- m
	}
	close(jobs)
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"checked": checked, "total": len(todo), "rate_limited": rateLimited})
}
