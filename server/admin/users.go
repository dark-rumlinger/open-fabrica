package admin

import (
	"net/http"

	"openfabrica/db/models"
	"openfabrica/security"
)

// UsersHandler handles user management endpoints.
type UsersHandler struct {
	deps *Deps
}

type createUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type updateUserRequest struct {
	Password    string `json:"password,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// List returns all users.
func (h *UsersHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := models.ListUsers(h.deps.DB.DB)
	if err != nil {
		h.deps.serverError(w, "failed to list users", err)
		return
	}

	if users == nil {
		users = []models.User{}
	}

	writeJSON(w, http.StatusOK, users)
}

// Create creates a new panel account. Every panel account is an admin.
func (h *UsersHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	if err := validateSetupCredentials(req.Username, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := security.HashPassword(req.Password)
	if err != nil {
		h.deps.Logger.Error("failed to hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	user, err := models.CreateUser(h.deps.DB.DB, req.Username, hash, "admin", req.DisplayName)
	if err != nil {
		h.deps.serverError(w, "failed to create user", err)
		return
	}

	writeJSON(w, http.StatusCreated, user)
}

// Update updates a user's password and/or display name.
func (h *UsersHandler) Update(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireIntParam(w, r, "userID", "invalid user ID")
	if !ok {
		return
	}

	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Password != "" {
		if err := validatePasswordComplexity(req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, err := security.HashPassword(req.Password)
		if err != nil {
			h.deps.Logger.Error("failed to hash password", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update password")
			return
		}
		if err := models.UpdateUserPassword(h.deps.DB.DB, userID, hash); err != nil {
			h.deps.Logger.Error("failed to update user password", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}

	if req.DisplayName != "" {
		if err := models.UpdateUserDisplayName(h.deps.DB.DB, userID, req.DisplayName); err != nil {
			h.deps.Logger.Error("failed to update display name", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}

	user, err := models.GetUserByID(h.deps.DB.DB, userID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// Delete removes a user. Refuses to delete the last admin.
func (h *UsersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireIntParam(w, r, "userID", "invalid user ID")
	if !ok {
		return
	}

	// Prevent deleting the last admin user.
	user, _ := models.GetUserByID(h.deps.DB.DB, userID)
	if user != nil && user.Role == "admin" {
		if models.CountAdminUsers(h.deps.DB.DB) <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete the last admin user")
			return
		}
	}

	if err := models.DeleteUser(h.deps.DB.DB, userID); err != nil {
		h.deps.serverError(w, "failed to delete user", err)
		return
	}

	writeStatus(w, "deleted")
}
