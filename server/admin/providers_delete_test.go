package admin

import (
	"net/http"
	"strconv"
	"testing"

	"openfabrica/db/models"
	"openfabrica/providers/llm"
)

// A project holds its model by id with no foreign key, so removing the provider or
// model out from under it must be refused, and a removed provider must stop
// answering from the live registry.
func TestDeleteProviderAndModelAreRefusedWhileInUse(t *testing.T) {
	deps := newBotTestDeps(t)
	deps.LLMRegistry = llm.NewRegistry()
	h := &ProvidersHandler{deps: deps}

	p, err := models.CreateProvider(deps.DB.DB, "Test LLM", "test-llm", "openai-chat", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.DB.Exec(`INSERT INTO llm_models (provider_id, model_id, display_name, context_tokens) VALUES (?, 'm1', 'M1', 8000)`, p.ID); err != nil {
		t.Fatal(err)
	}
	var modelPK int
	if err := deps.DB.DB.QueryRow(`SELECT id FROM llm_models WHERE provider_id = ?`, p.ID).Scan(&modelPK); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.DB.Exec(`INSERT INTO projects (name, llm_model_id, mode) VALUES ('uses it', ?, 'building')`, modelPK); err != nil {
		t.Fatal(err)
	}

	pid, mid := strconv.Itoa(p.ID), strconv.Itoa(modelPK)
	if rec := doJSON(h.DeleteModel, "DELETE", "/m", "", map[string]string{"providerID": pid, "modelID": mid}); rec.Code != http.StatusConflict {
		t.Errorf("deleting a model a project runs: %d, want 409", rec.Code)
	}
	if rec := doJSON(h.Delete, "DELETE", "/p", "", map[string]string{"providerID": pid}); rec.Code != http.StatusConflict {
		t.Errorf("deleting a provider a project uses: %d, want 409", rec.Code)
	}

	// Once the project moves off it, the delete goes through and the live instance goes too.
	if _, err := deps.DB.DB.Exec(`DELETE FROM projects`); err != nil {
		t.Fatal(err)
	}
	deps.LLMRegistry.Register("Test LLM", nil)
	if rec := doJSON(h.Delete, "DELETE", "/p", "", map[string]string{"providerID": pid}); rec.Code != http.StatusOK {
		t.Fatalf("delete after the project moved on: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := deps.LLMRegistry.Get("Test LLM"); err == nil {
		t.Error("a deleted provider is still in the live registry")
	}
}
