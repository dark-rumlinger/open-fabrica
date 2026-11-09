package models

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Project modes: the owner-facing pipeline mode stored in projects.mode. The
// engine, admin API, Telegram bot, and manage_project all read and write it, so
// the values live here with the column rather than as literals at each site.
const (
	ModeBuilding = "building"
	ModeLive     = "live"
	ModePaused   = "paused"
)

// ValidMode reports whether mode is one of the stored project modes.
func ValidMode(mode string) bool {
	switch mode {
	case ModeBuilding, ModeLive, ModePaused:
		return true
	}
	return false
}

type Project struct {
	ID              int       `json:"id"`
	Name            string    `json:"name"`
	Domain          *string   `json:"domain"`
	Description     *string   `json:"description"`
	LLMModelID      int       `json:"llm_model_id"`
	Published       bool      `json:"published"`
	Mode            string    `json:"mode"`
	Config          string    `json:"config"`
	SourceProjectID *int      `json:"source_project_id"`
	AmbitionLevel   string    `json:"ambition_level"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const projectColumns = "id, name, domain, description, llm_model_id, published, mode, config, source_project_id, ambition_level, created_at, updated_at"

func scanProject(s *Project, row interface{ Scan(...any) error }) error {
	return row.Scan(&s.ID, &s.Name, &s.Domain, &s.Description, &s.LLMModelID, &s.Published, &s.Mode, &s.Config, &s.SourceProjectID, &s.AmbitionLevel, &s.CreatedAt, &s.UpdatedAt)
}

func CreateProject(db *sql.DB, name string, domain, description *string, llmModelID int, sourceProjectID *int, ambitionLevel string) (*Project, error) {
	result, err := db.Exec(
		"INSERT INTO projects (name, domain, description, llm_model_id, published, mode, source_project_id, ambition_level) VALUES (?, ?, ?, ?, 1, 'building', ?, ?)",
		name, domain, description, llmModelID, sourceProjectID, ambitionLevel,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading new project id: %w", err)
	}
	return GetProjectByID(db, int(id))
}

func GetProjectByID(db *sql.DB, id int) (*Project, error) {
	s := &Project{}
	err := scanProject(s, db.QueryRow("SELECT "+projectColumns+" FROM projects WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ProjectNameExists reports whether any project already uses the given name.
// The name column has no UNIQUE constraint, so generated names are checked
// against this before use.
func ProjectNameExists(db *sql.DB, name string) (bool, error) {
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE name = ?)", name).Scan(&exists)
	return exists, err
}

// ListProjectNames returns every project's name. Used to feed the full
// taken-name set into title generation up front, so a name clash is avoided in
// the first call instead of discovered through retries.
func ListProjectNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM projects")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func GetProjectByDomain(db *sql.DB, domain string) (*Project, error) {
	s := &Project{}
	err := scanProject(s, db.QueryRow("SELECT "+projectColumns+" FROM projects WHERE domain = ?", domain))
	if err != nil {
		return nil, err
	}
	return s, nil
}

func ListProjects(db *sql.DB) ([]Project, error) {
	rows, err := db.Query("SELECT " + projectColumns + " FROM projects ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var s Project
		if err := scanProject(&s, rows); err != nil {
			return nil, err
		}
		projects = append(projects, s)
	}
	return projects, rows.Err()
}

// ListProjectsPaginated returns a page of projects with the total count.
func ListProjectsPaginated(db *sql.DB, limit, offset int) ([]Project, int, error) {
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM projects").Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.Query("SELECT "+projectColumns+" FROM projects ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var s Project
		if err := scanProject(&s, rows); err != nil {
			return nil, 0, err
		}
		projects = append(projects, s)
	}
	return projects, total, rows.Err()
}

// listProjectsWhere returns the projects matching a fixed WHERE condition, by id.
func listProjectsWhere(db *sql.DB, cond string) ([]Project, error) {
	rows, err := db.Query("SELECT " + projectColumns + " FROM projects WHERE " + cond + " ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var s Project
		if err := scanProject(&s, rows); err != nil {
			return nil, err
		}
		projects = append(projects, s)
	}
	return projects, rows.Err()
}

// ListPublishedProjects returns the projects served to visitors. The firewall
// check, the TLS proxy and the DNS watch work from this set, because they exist
// to make published sites reachable.
func ListPublishedProjects(db *sql.DB) ([]Project, error) {
	return listProjectsWhere(db, "published = 1")
}

// ListRunnableProjects returns the projects whose background work (the engine
// worker, scheduled tasks, the job runner) starts with the server. Publishing
// does not decide this, so hiding a project mid-build does not stop its build.
// The one project skipped is one that is both unpublished and paused: that is
// what an import lands as, and nothing it carries should run until the owner
// has reviewed it.
func ListRunnableProjects(db *sql.DB) ([]Project, error) {
	return listProjectsWhere(db, "published = 1 OR mode != '"+ModePaused+"'")
}

func UpdateProject(db *sql.DB, id int, name string, domain, description *string, llmModelID int) error {
	_, err := db.Exec(
		"UPDATE projects SET name = ?, domain = ?, description = ?, llm_model_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		name, domain, description, llmModelID, id,
	)
	return err
}

// SetProjectPublished publishes or unpublishes a project.
func SetProjectPublished(db *sql.DB, id int, published bool) error {
	_, err := db.Exec(
		"UPDATE projects SET published = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		published, id,
	)
	return err
}

func UpdateProjectMode(db *sql.DB, id int, mode string) error {
	_, err := db.Exec(
		"UPDATE projects SET mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		mode, id,
	)
	return err
}

// UpdateProjectName updates only the display name. Used by the engine when it
// replaces an auto-minted name with an LLM-generated title; the domain/slug is
// deliberately left untouched so existing routing keeps working.
func UpdateProjectName(db *sql.DB, id int, name string) error {
	_, err := db.Exec(
		"UPDATE projects SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		name, id,
	)
	return err
}

// UpdateProjectConfig overwrites the config JSON blob.
func UpdateProjectConfig(db *sql.DB, id int, config string) error {
	_, err := db.Exec(
		"UPDATE projects SET config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		config, id,
	)
	return err
}

// projectConfigMu serializes mutateProjectConfig's read-modify-write.
var projectConfigMu sync.Mutex

// mutateProjectConfig reads the project's config blob, applies mutate to the
// decoded map, and writes it back, preserving keys the caller does not touch. A
// non-empty blob that fails to parse is an error, not silently discarded:
// overwriting it wholesale would drop other stored keys (banned IPs, CSP, auto-name).
func mutateProjectConfig(db *sql.DB, id int, mutate func(m map[string]any)) error {
	// Read-modify-write on one blob: two concurrent mutations (the owner saving
	// settings while the engine clears the auto-name flag) would each read the old
	// blob and the later write would drop the earlier key. This is the only writer
	// of the blob, so one process-wide lock closes the race.
	projectConfigMu.Lock()
	defer projectConfigMu.Unlock()
	p, err := GetProjectByID(db, id)
	if err != nil {
		return err
	}
	m := map[string]any{}
	if s := strings.TrimSpace(p.Config); s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return fmt.Errorf("project %d has an unparseable config blob; refusing to overwrite: %w", id, err)
		}
	}
	mutate(m)
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return UpdateProjectConfig(db, id, string(buf))
}

// projectConfigAutoNameKey flags, inside projects.config, that the project's name
// was auto-minted rather than typed by the owner. The engine reads it to decide
// whether to replace the name with a generated title, then clears it.
const projectConfigAutoNameKey = "name_auto_generated"

// ProjectNameIsAutoGenerated reports whether the project's name was auto-minted.
func ProjectNameIsAutoGenerated(p *Project) bool {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return false
	}
	b, _ := m[projectConfigAutoNameKey].(bool)
	return b
}

// SetProjectNameAutoGenerated sets/clears the auto-name flag in projects.config,
// preserving any other keys already stored there.
func SetProjectNameAutoGenerated(db *sql.DB, id int, v bool) error {
	return mutateProjectConfig(db, id, func(m map[string]any) {
		m[projectConfigAutoNameKey] = v
	})
}

// projectConfigBannedIPsKey holds, inside projects.config, the owner-managed
// list of IP addresses and CIDR ranges that are blocked from the public site.
// Entries are validated at save time; the public IP-ban middleware reads them.
const projectConfigBannedIPsKey = "banned_ips"

// ProjectBannedIPs returns the project's banned IP/CIDR list from config, or
// nil if none are set or the blob is empty/unparseable.
func ProjectBannedIPs(p *Project) []string {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return nil
	}
	raw, ok := m[projectConfigBannedIPsKey].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SetProjectBannedIPs stores the banned IP/CIDR list in projects.config,
// preserving any other keys already there. An empty list clears the key.
func SetProjectBannedIPs(db *sql.DB, id int, ips []string) error {
	return mutateProjectConfig(db, id, func(m map[string]any) {
		if len(ips) == 0 {
			delete(m, projectConfigBannedIPsKey)
		} else {
			m[projectConfigBannedIPsKey] = ips
		}
	})
}

// projectConfigDeployPolicyKey holds, inside projects.config, whether a build
// carrying blocking findings may go live. Empty/unset means "use the platform
// default"; the engine owns the value set and default, so this stores only the
// owner's override. See engine/blocking.go.
const projectConfigDeployPolicyKey = "deploy_policy"

// ProjectDeployPolicy returns the project's deploy-policy override from config, or
// "" when unset (use the platform default). The engine validates the value; an
// unrecognized string is returned as-is and rejected there.
func ProjectDeployPolicy(p *Project) string {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return ""
	}
	s, _ := m[projectConfigDeployPolicyKey].(string)
	return s
}

// SetProjectDeployPolicy stores (or, on empty input, clears) the owner's
// deploy-policy override in projects.config, preserving any other keys there.
func SetProjectDeployPolicy(db *sql.DB, id int, policy string) error {
	return mutateProjectConfig(db, id, func(m map[string]any) {
		if strings.TrimSpace(policy) == "" {
			delete(m, projectConfigDeployPolicyKey)
			return
		}
		m[projectConfigDeployPolicyKey] = policy
	})
}

// projectConfigUserFlowsKey holds, inside projects.config, whether the project runs
// user-flow tests (of_test.js), an experimental feature. The owner's choice is stored
// explicitly, on or off; unset means the platform default, which is off.
const projectConfigUserFlowsKey = "user_flows"

// ProjectUserFlowsEnabled reports whether the project runs user-flow tests. It is false
// unless the owner explicitly turned them on.
func ProjectUserFlowsEnabled(p *Project) bool {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return false
	}
	on, _ := m[projectConfigUserFlowsKey].(bool)
	return on
}

// SetProjectUserFlows stores the owner's choice in projects.config, preserving any
// other keys there. Both values are stored, so a choice stays leading if the default
// ever changes again.
func SetProjectUserFlows(db *sql.DB, id int, enabled bool) error {
	return mutateProjectConfig(db, id, func(m map[string]any) {
		m[projectConfigUserFlowsKey] = enabled
	})
}

// projectConfigCSPKey holds, inside projects.config, an optional per-project
// Content-Security-Policy overriding the instance default on this project's public
// HTML responses. Empty/unset = use the instance default.
const projectConfigCSPKey = "csp"

// ProjectCSP returns the project's CSP override from config, or "" if unset.
func ProjectCSP(p *Project) string {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return ""
	}
	s, _ := m[projectConfigCSPKey].(string)
	return s
}

// SetProjectCSP stores (or, on empty input, clears) the per-project CSP override
// in projects.config, preserving any other keys already there.
func SetProjectCSP(db *sql.DB, id int, csp string) error {
	return mutateProjectConfig(db, id, func(m map[string]any) {
		if strings.TrimSpace(csp) == "" {
			delete(m, projectConfigCSPKey)
		} else {
			m[projectConfigCSPKey] = csp
		}
	})
}

// DeleteProject removes the project row from the global database.
// Per-project DB and file cleanup is handled by ProjectDBManager.Delete() in the admin handler.
func DeleteProject(db *sql.DB, id int) error {
	res, err := db.Exec("DELETE FROM projects WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %d not found", id)
	}
	return nil
}

// projectConfigReplyLanguageKey holds, inside projects.config, the directive for the
// language the assistant writes to the owner in: "" (unset), "match", or a language
// code. It is the last value recorded from a request that woke the engine, so the
// prompt builders read it from the database and a restarted worker reproduces the
// same prompt bytes.
const projectConfigReplyLanguageKey = "reply_language"

// ProjectReplyLanguage returns the project's stored reply-language directive, or ""
// when none is set.
func ProjectReplyLanguage(p *Project) string {
	if p == nil || strings.TrimSpace(p.Config) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Config), &m); err != nil {
		return ""
	}
	s, _ := m[projectConfigReplyLanguageKey].(string)
	return s
}

// SetProjectReplyLanguage records the directive, preserving every other config key.
// It writes only when the value changes: a chat message is the common caller, and
// an unchanged directive must not touch the row or its updated_at.
func SetProjectReplyLanguage(db *sql.DB, id int, directive string) error {
	p, err := GetProjectByID(db, id)
	if err != nil {
		return err
	}
	if ProjectReplyLanguage(p) == directive {
		return nil
	}
	return mutateProjectConfig(db, id, func(m map[string]any) {
		if directive == "" {
			delete(m, projectConfigReplyLanguageKey)
		} else {
			m[projectConfigReplyLanguageKey] = directive
		}
	})
}
