package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"openfabrica/store"
	"openfabrica/webhooks"
)

type ProjectWebhooksHandler struct {
	deps *Deps
}

func (h *ProjectWebhooksHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListWebhooks(projectDB))
}

func (h *ProjectWebhooksHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	wh, err := store.GetWebhook(projectDB, webhookID)
	if errors.Is(err, store.ErrWebhookNotFound) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load webhook")
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

func (h *ProjectWebhooksHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	isEnabled, err := store.ToggleWebhook(projectDB, webhookID)
	if errors.Is(err, store.ErrWebhookNotFound) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to toggle webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": webhookID, "is_enabled": isEnabled})
}

func (h *ProjectWebhooksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	if err := store.DeleteWebhook(projectDB, webhookID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete webhook")
		return
	}
	writeStatus(w, "deleted")
}

// Test sends one signed test event to an outgoing webhook's URL, from the
// server. The attempt is recorded in the webhook's delivery log.
func (h *ProjectWebhooksHandler) Test(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}

	target, err := store.LoadWebhookTarget(projectDB, webhookID)
	if errors.Is(err, store.ErrWebhookNotFound) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load webhook")
		return
	}
	if target.Direction == "incoming" {
		writeError(w, http.StatusBadRequest, "only outgoing webhooks can be tested; incoming ones receive requests")
		return
	}
	if target.URL == "" {
		writeError(w, http.StatusBadRequest, "this webhook has no URL to send to")
		return
	}

	secret := ""
	if target.SecretEncrypted != "" {
		opened, openErr := h.deps.Encryptor.OpenOptional(target.SecretEncrypted)
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "the signing secret cannot be decrypted")
			return
		}
		secret = opened
	}

	result := webhooks.SendTest(r.Context(), target.URL, secret, projectID, target.Name)
	store.RecordWebhookTestLog(projectDB, webhookID, "test event", result.StatusCode, result.Response, result.Success)
	writeJSON(w, http.StatusOK, result)
}

type updateWebhookRequest struct {
	Name   *string   `json:"name"`
	URL    *string   `json:"url"`
	Secret *string   `json:"secret"`
	Events *[]string `json:"events"`
}

// Update modifies an existing webhook.
func (h *ProjectWebhooksHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}

	var req updateWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var sealedPtr *string
	if req.Secret != nil {
		sealed, err := h.deps.Encryptor.SealOptional(*req.Secret)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to protect webhook secret")
			return
		}
		sealedPtr = &sealed
	}

	if err := store.UpdateWebhook(projectDB, webhookID, store.WebhookUpdate{
		Name:            req.Name,
		URL:             req.URL,
		SecretEncrypted: sealedPtr,
		Events:          req.Events,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": webhookID, "updated": true})
}

type createWebhookRequest struct {
	Name      string   `json:"name"`
	Direction string   `json:"direction"`
	URL       string   `json:"url"`
	Secret    string   `json:"secret"`
	Events    []string `json:"events"`
}

// Create adds a new webhook for a project.
func (h *ProjectWebhooksHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var req createWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Direction == "" {
		req.Direction = "incoming"
	}
	if req.Direction == "outgoing" && req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required for outgoing webhooks")
		return
	}

	secret := req.Secret
	if secret == "" {
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to generate webhook secret")
			return
		}
		secret = hex.EncodeToString(secretBytes)
	}

	sealed, err := h.deps.Encryptor.SealOptional(secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to protect webhook secret")
		return
	}

	whID, err := store.CreateWebhook(projectDB, req.Name, req.Direction, req.URL, sealed, req.Events)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": whID, "name": req.Name, "secret": secret})
}
