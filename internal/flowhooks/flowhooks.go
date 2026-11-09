// Package flowhooks gives user-flow replays (of_test.js, see probe) the parts of a
// project a browser cannot reach: the messages the app sent during the replay,
// accounts with a role a signup cannot produce, the project's scheduled tasks and
// its inbound webhooks. The tools (write_flows / test_flows) and the build stage's
// flow validator put one on the context with probe.WithFlowHooks.
package flowhooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"openfabrica/internal/dbh"
	"openfabrica/internal/replayoutbox"
	"openfabrica/probe"
	"openfabrica/security"
)

// Hooks implements probe.FlowHooks for one project.
type Hooks struct {
	projectID int
	db        dbh.Handle // the project's database (writes go to its write pool)
	jwt       *security.JWTManager
	enc       *security.Encryptor // opens webhook signing secrets
}

// New returns the hooks for a project. db must accept writes (seeded users); jwt
// may be nil, in which case seeded users cannot start signed in; enc may be nil,
// in which case a webhook step cannot sign (it reports why).
func New(projectID int, db dbh.Handle, jwt *security.JWTManager, enc *security.Encryptor) *Hooks {
	return &Hooks{projectID: projectID, db: db, jwt: jwt, enc: enc}
}

var _ probe.FlowHooks = (*Hooks)(nil)

// Begin captures the project's outgoing email and push for the replay.
func (h *Hooks) Begin() func() { return replayoutbox.Begin(h.projectID) }

// Messages returns what the app sent since Begin.
func (h *Hooks) Messages() []probe.FlowMessage {
	msgs := replayoutbox.Messages(h.projectID)
	out := make([]probe.FlowMessage, len(msgs))
	for i, m := range msgs {
		out[i] = probe.FlowMessage{Kind: m.Kind, To: m.To, Subject: m.Subject, Body: m.Body}
	}
	return out
}

// SeedUser creates an account in the project's (first) auth table: the username
// column gets the email when it holds emails, the password is hashed as a signup
// would, and the role column gets the role (the endpoint's default when empty). A
// row left over with the same name is replaced. The replay's snapshot restore
// removes the account afterwards.
func (h *Hooks) SeedUser(_ context.Context, u probe.FlowSeedUser) (string, error) {
	var table, userCol, passCol, defaultRole, roleCol string
	err := h.db.QueryRow(`SELECT table_name, username_column, password_column, COALESCE(default_role,''), COALESCE(role_column,'')
		FROM of_auth_endpoints ORDER BY id LIMIT 1`).Scan(&table, &userCol, &passCol, &defaultRole, &roleCol)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("the project has no sign-in (no auth endpoint), so there is no account to create; drop users from the flow")
	}
	if err != nil {
		return "", fmt.Errorf("reading the auth endpoint: %w", err)
	}
	if roleCol == "" {
		roleCol = "role"
	}
	if defaultRole == "" {
		defaultRole = "user"
	}
	if err := security.ValidateTableName(table); err != nil {
		return "", err
	}
	for _, c := range []string{userCol, passCol, roleCol} {
		if err := security.ValidateColumnName(c); err != nil {
			return "", err
		}
	}
	cols, err := tableColumns(h.db, table)
	if err != nil {
		return "", err
	}
	role := u.Role
	if role == "" {
		role = defaultRole
	}
	login := u.Username
	if strings.Contains(strings.ToLower(userCol), "mail") {
		login = u.Email
	}
	hash, err := security.HashPassword(u.Password)
	if err != nil {
		return "", err
	}
	values := map[string]any{userCol: login, passCol: hash}
	if _, ok := cols[roleCol]; ok {
		values[roleCol] = role
	}
	for name, v := range map[string]string{"email": u.Email, "username": u.Username} {
		if _, ok := cols[name]; ok && values[name] == nil {
			values[name] = v
		}
	}
	// Other required columns get a neutral value, as a signup form would have
	// asked for them (a display name, a count).
	for name, c := range cols {
		if _, set := values[name]; set || !c.required {
			continue
		}
		if c.numeric {
			values[name] = 0
		} else {
			values[name] = "Flow test"
		}
	}
	if _, err := h.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s = ?", security.QuoteIdent(table), security.QuoteIdent(userCol)), login); err != nil {
		return "", fmt.Errorf("clearing a leftover account: %w", err)
	}
	names := make([]string, 0, len(values))
	marks := make([]string, 0, len(values))
	args := make([]any, 0, len(values))
	for name, v := range values {
		names = append(names, security.QuoteIdent(name))
		marks = append(marks, "?")
		args = append(args, v)
	}
	res, err := h.db.Exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", security.QuoteIdent(table),
		strings.Join(names, ", "), strings.Join(marks, ", ")), args...)
	if err != nil {
		return "", fmt.Errorf("creating the account in %s: %w", table, err)
	}
	id, _ := res.LastInsertId()
	if h.jwt == nil {
		return "", nil
	}
	return h.jwt.Generate(security.TokenRequest{ProjectID: h.projectID, UserID: int(id), Username: login, Role: role, Expiry: time.Hour})
}

type column struct{ required, numeric bool }

// tableColumns reads a table's columns: whether each must be given a value (NOT
// NULL, no default, not the rowid key) and whether it holds numbers.
func tableColumns(db dbh.Handle, table string) (map[string]column, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", security.QuoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]column{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		t := strings.ToUpper(typ)
		out[name] = column{
			required: notNull == 1 && !dflt.Valid && pk == 0,
			numeric:  strings.Contains(t, "INT") || strings.Contains(t, "REAL") || strings.Contains(t, "NUM") || strings.Contains(t, "FLOA") || strings.Contains(t, "DOUB"),
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the auth table %q does not exist", table)
	}
	return out, rows.Err()
}

// WebhookSignature signs body with the named inbound webhook's secret, in the
// X-Webhook-Signature form the platform verifies.
func (h *Hooks) WebhookSignature(name string, body []byte) (string, error) {
	var sealed string
	var enabled bool
	err := h.db.QueryRow(`SELECT COALESCE(secret_encrypted,''), is_enabled FROM of_webhooks WHERE name = ?`, name).Scan(&sealed, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("the project has no inbound webhook named %q (manage_webhooks create without a url)", name)
	}
	if err != nil {
		return "", err
	}
	secret, err := h.enc.OpenOptional(sealed)
	if err != nil {
		return "", fmt.Errorf("webhook %q has a signing secret that cannot be decrypted: %w", name, err)
	}
	if secret == "" {
		return "", fmt.Errorf("webhook %q has no signing secret, so the platform rejects every call to it", name)
	}
	return "sha256=" + security.HMACSHA256Hex([]byte(secret), body), nil
}

// ScheduleRunner runs a project's scheduled task by name and waits for it to
// finish. The engine registers it (it owns the workers that execute tasks).
type ScheduleRunner func(ctx context.Context, projectID int, name string) error

var (
	runnerMu sync.RWMutex
	runner   ScheduleRunner
)

// SetScheduleRunner registers how runSchedule steps execute a task.
func SetScheduleRunner(fn ScheduleRunner) {
	runnerMu.Lock()
	runner = fn
	runnerMu.Unlock()
}

// RunSchedule runs the named scheduled task now.
func (h *Hooks) RunSchedule(ctx context.Context, name string) error {
	runnerMu.RLock()
	fn := runner
	runnerMu.RUnlock()
	if fn == nil {
		return errors.New("scheduled tasks cannot be run from this replay (no engine here)")
	}
	return fn(ctx, h.projectID, name)
}
