package admin

import (
	"net/http"

	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/replylang"
	"openfabrica/security"
)

// claimsUserID returns the signed-in user's id from the request context.
func claimsUserID(r *http.Request) (int, bool) {
	claims, ok := r.Context().Value(security.ClaimsKey).(*security.Claims)
	if !ok || claims == nil {
		return 0, false
	}
	return claims.UserID, true
}

// recordReplyLanguage stores, on the project, the language the assistant should
// write in for the request that is about to wake its engine: the signed-in user's
// preference resolved against the interface language the browser reported.
//
// The last person to act on a project sets the language, which is what a shared
// project wants: the reply goes to whoever just asked.
//
// This never fails the request. A language is a courtesy on top of the action the
// user actually asked for, so a lookup or write problem is logged and the project
// keeps the language it had. Call it before the engine is told to run, so the
// prompt it builds already carries the new value.
func (d *Deps) recordReplyLanguage(r *http.Request, projectID int) {
	userID, ok := claimsUserID(r)
	if !ok {
		return
	}
	pref, err := models.GetUserAssistantLanguage(d.DB.DB, userID)
	if err != nil {
		d.Logger.Warn("reply language: could not read the user's preference", "user_id", userID, "error", err)
		return
	}
	directive, decided := replylang.Resolve(pref, r.Header.Get(replylang.Header))
	if !decided {
		return
	}
	if err := models.SetProjectReplyLanguage(d.DB.DB, projectID, directive); err != nil {
		d.Logger.Warn("reply language: could not store the project's language", "project_id", projectID, "error", err)
	}
}

// PreferencesHandler serves the signed-in user's own preferences. Every role may
// read and change their own; nothing here reaches another user.
type PreferencesHandler struct {
	deps *Deps
}

type preferencesBody struct {
	AssistantLanguage string `json:"assistant_language"`
}

// Get returns the user's preferences.
//
//	GET /admin/api/auth/preferences
func (h *PreferencesHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, ok := claimsUserID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing authorization")
		return
	}
	lang, err := models.GetUserAssistantLanguage(h.deps.DB.DB, userID)
	if err != nil {
		h.deps.serverError(w, "failed to load preferences", err)
		return
	}
	// A row written by a newer or hand-edited build must not poison the client's
	// select; an unrecognized value reads as the default.
	if canon, valid := replylang.NormalizePreference(lang); valid {
		lang = canon
	} else {
		lang = replylang.Interface
	}
	writeJSON(w, http.StatusOK, preferencesBody{AssistantLanguage: lang})
}

// Update stores the user's preferences and answers with the stored form.
//
//	PUT /admin/api/auth/preferences
func (h *PreferencesHandler) Update(w http.ResponseWriter, r *http.Request) {
	userID, ok := claimsUserID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing authorization")
		return
	}
	var req preferencesBody
	if !decodeJSON(w, r, &req) {
		return
	}
	lang, valid := replylang.NormalizePreference(req.AssistantLanguage)
	if !valid {
		writeError(w, http.StatusBadRequest, "assistant_language must be empty, \"match\", or a supported language code")
		return
	}
	if err := models.SetUserAssistantLanguage(h.deps.DB.DB, userID, lang); err != nil {
		h.deps.serverError(w, "failed to save preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, preferencesBody{AssistantLanguage: lang})
}

// localizeAll translates each message in the caller's language, keeping the slice
// nil when it was nil so an absent list stays absent in the JSON.
func localizeAll(r *http.Request, msgs []string) []string {
	if msgs == nil {
		return nil
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = msgcat.Localize(r, m)
	}
	return out
}
