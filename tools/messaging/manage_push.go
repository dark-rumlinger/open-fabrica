package messaging

import (
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/internal/netguard"
	"openfabrica/internal/replayoutbox"
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// manage_push registers per-project Web Push (VAPID) endpoints. The act of
// creating an endpoint mints a VAPID keypair, stores the private half in
// the encrypted secrets table, and exposes /api/<path>/push/subscribe for
// browsers to register PushSubscriptions against.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_push",
		Category: "messaging",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult:          8000,
		RequiresCapability: "push",
		OneLiner:           "Send Web Push notifications - VAPID keys, /push/subscribe endpoint, subscriber sends.",
		WhenToUse: `Use when the project asks for browser push (e.g. news alerts, order updates).
Create an endpoint, integrate the subscribe flow in the page JS, then call
send to broadcast or target one user_id; list_subscribers shows who has
registered. This is OS-level browser push - in-app toast/inbox messages belong
on manage_notifications, and email on its own tool.`,
		Actions: []tools.Action{
			&PushCreate{},
			&PushUpdate{},
			&PushList{},
			&PushDelete{},
			&PushSend{},
			&PushListSubscribers{},
		},
		Hints: func(action string, args map[string]any, r *tools.Result) string {
			switch action {
			case "create":
				if r != nil && r.Success {
					p, _ := args["endpoint_path"].(string)
					return fmt.Sprintf("Push endpoint ready. Browser flow: the notification is shown by the platform service worker /service-worker.js, which manage_project enable_pwa creates (as the last build step, once every page is saved; never hand-write one). Register it with navigator.serviceWorker.register('/service-worker.js'), then call navigator.serviceWorker.ready.then(reg => reg.pushManager.subscribe({userVisibleOnly:true, applicationServerKey:<urlBase64ToUint8Array(VAPID_PUBLIC_KEY)>})), POST the resulting subscription JSON to /api/%s/push/subscribe.", p)
				}
			case "send":
				return "send needs endpoint_path and title (message_body is optional). Pass user_id to target one subscriber, omit it to broadcast."
			case "list_subscribers":
				return "Returns one row per active subscription for endpoint_path (newest 200). A non-empty list confirms opt-ins are landing before you call send."
			}
			return ""
		},
	})
}

// PushCreate mints a VAPID keypair for endpoint_path. Re-calling with the
// same path is idempotent: returns the existing public key without rotating
// (rotating would invalidate every PushSubscription already on file).
type PushCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Path suffix (e.g. 'news' -> /api/news/push/subscribe)."`
	ContactEmail string `arg:"contact_email"          desc:"VAPID 'mailto:' subject (abuse contact). Recommended; falls back to a reserved placeholder domain when unset."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Require an authenticated session to subscribe."`
}

func (a *PushCreate) Doc() string { return "Mint a Web Push endpoint with its own VAPID keypair." }
func (a *PushCreate) Example() any {
	return &PushCreate{EndpointPath: "news", ContactEmail: "ops@example.com"}
}
func (a *PushCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	var existingPub string
	if err := ctx.DB.QueryRow("SELECT vapid_public_key FROM of_push_endpoints WHERE path = ?", path).Scan(&existingPub); err == nil {
		return tools.Ok(map[string]any{
			"endpoint_path": path,
			"public_key":    existingPub,
			"subscribe_url": "/api/" + path + "/push/subscribe",
			"message":       "endpoint already exists, returning existing public key",
		}), nil
	}

	pubB64, privB64, err := GenerateVAPIDKeypair()
	if err != nil {
		return nil, fmt.Errorf("generating VAPID keypair: %w", err)
	}
	if ctx.Encryptor == nil {
		return tools.Fail("encryption not available"), nil
	}
	encPriv, err := ctx.Encryptor.Encrypt(privB64)
	if err != nil {
		return nil, fmt.Errorf("encrypting VAPID private key: %w", err)
	}
	secretName := "vapid_private_" + path
	if _, err := ctx.DB.Exec(`
		INSERT INTO of_secrets (name, value_encrypted)
		VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		secretName, encPriv); err != nil {
		return nil, fmt.Errorf("storing VAPID secret: %w", err)
	}
	if err := api.ClaimEndpoint(ctx, endpoints.KindPush, path, nil); err != nil {
		return tools.FailErr(err), nil
	}
	// The existing-endpoint case returned above, so this row is always new: a plain
	// INSERT. contact_email / requires_auth are changed afterwards with action=update
	// (create never rotates the keypair, so it cannot double as an updater).
	if _, err := ctx.DB.Exec(`
		INSERT INTO of_push_endpoints (path, vapid_public_key, vapid_private_secret_name, contact_email, requires_auth)
		VALUES (?, ?, ?, ?, ?)`,
		path, pubB64, secretName, a.ContactEmail, a.RequiresAuth); err != nil {
		return nil, fmt.Errorf("creating push endpoint: %w", err)
	}
	return tools.Ok(map[string]any{
		"endpoint_path": path,
		"public_key":    pubB64,
		"subscribe_url": "/api/" + path + "/push/subscribe",
	}), nil
}

// PushUpdate changes an existing push endpoint's contact_email / requires_auth.
// Create cannot double as an updater (it returns early on an existing path to avoid
// rotating the keypair), so this is the only way to change those after creation.
type PushUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Path of the push endpoint to update."`
	ContactEmail string `arg:"contact_email"          desc:"Replace the VAPID 'mailto:' abuse contact. Omit (or empty) to leave unchanged."`
	RequiresAuth *bool  `arg:"requires_auth"          desc:"Toggle whether subscribing needs an authenticated session (true/false). Omit to leave unchanged."`
}

func (a *PushUpdate) Doc() string {
	return "Update a push endpoint's contact email or auth requirement."
}
func (a *PushUpdate) Example() any {
	return &PushUpdate{EndpointPath: "news", ContactEmail: "ops@example.com"}
}
func (a *PushUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	var curContact string
	var curAuth bool
	if err := ctx.DB.QueryRow("SELECT contact_email, requires_auth FROM of_push_endpoints WHERE path = ?", path).Scan(&curContact, &curAuth); err != nil {
		return tools.Fail("push endpoint '%s' not found - create it first with action=create", path), nil
	}
	var sets []string
	var args []any
	if a.ContactEmail != "" {
		sets = append(sets, "contact_email = ?")
		args = append(args, a.ContactEmail)
	}
	if a.RequiresAuth != nil {
		sets = append(sets, "requires_auth = ?")
		args = append(args, *a.RequiresAuth)
	}
	if len(sets) == 0 {
		return tools.Fail("nothing to update: pass contact_email and/or requires_auth"), nil
	}
	args = append(args, path)
	if _, err := ctx.DB.Exec("UPDATE of_push_endpoints SET "+strings.Join(sets, ", ")+" WHERE path = ?", args...); err != nil {
		return nil, fmt.Errorf("updating push endpoint: %w", err)
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

// PushList enumerates the project's push endpoints.
type PushList struct{}

func (a *PushList) Doc() string    { return "List every push endpoint in the project." }
func (a *PushList) ReadOnly() bool { return true }
func (a *PushList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`
		SELECT id, path, vapid_public_key, contact_email, requires_auth, created_at
		FROM of_push_endpoints ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var path, pub, contact string
		var ra bool
		var created time.Time
		if err := rows.Scan(&id, &path, &pub, &contact, &ra, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "public_key": pub,
			"contact_email": contact, "requires_auth": ra, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}

// PushDelete removes a push endpoint by path through the shared
// api.DeleteEndpointByPath helper so we get the same path normalization,
// not-found shape, and destructive-action logging as every other endpoint
// deleter in the catalogue.
type PushDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Path of the push endpoint to remove."`
}

func (a *PushDelete) Doc() string  { return "Remove a push endpoint by path." }
func (a *PushDelete) Example() any { return &PushDelete{EndpointPath: "news"} }
func (a *PushDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return api.DeleteEndpointByPath(ctx, "of_push_endpoints", "push endpoint", "manage_push", "delete", a.EndpointPath)
}

// PushSend pushes a notification to one user or every subscriber of an
// endpoint. Failed deliveries that hit a 404/410 ("subscription gone") get
// deleted so subsequent broadcasts skip dead subscriptions.
type PushSend struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Path of the push endpoint to broadcast on."`
	Title        string `arg:"title,required"         desc:"Notification title."`
	MessageBody  string `arg:"message_body"           desc:"Notification body text."`
	URL          string `arg:"url"                    desc:"Click target URL."`
	UserID       int    `arg:"user_id"                desc:"Optional: target a single subscriber."`
}

func (a *PushSend) Doc() string {
	return "Push a notification to one or every subscriber of an endpoint."
}
func (a *PushSend) Example() any {
	return &PushSend{EndpointPath: "news", Title: "New article", MessageBody: "Open to read."}
}
func (a *PushSend) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" || a.Title == "" {
		return tools.Fail("endpoint_path and title are required"), nil
	}
	var pubKey, privSecretName, contact string
	if err := ctx.DB.QueryRow(
		`SELECT vapid_public_key, vapid_private_secret_name, contact_email FROM of_push_endpoints WHERE path = ?`,
		path,
	).Scan(&pubKey, &privSecretName, &contact); err != nil {
		return tools.Fail("push endpoint '%s' not found", path), nil
	}
	var privEnc string
	if err := ctx.DB.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", privSecretName).Scan(&privEnc); err != nil {
		return tools.Fail("VAPID private key missing"), nil
	}
	if ctx.Encryptor == nil {
		return tools.Fail("encryption not available"), nil
	}
	privKey, err := ctx.Encryptor.Decrypt(privEnc)
	if err != nil {
		return tools.Fail("decrypting VAPID key failed"), nil
	}

	// A user-flow replay keeps the notification instead of pushing it to real
	// subscribers (see replayoutbox).
	to := path
	if a.UserID > 0 {
		to = fmt.Sprintf("%s user %d", path, a.UserID)
	}
	if replayoutbox.Capture(ctx.ProjectID, replayoutbox.Message{Kind: "push", To: to, Subject: a.Title, Body: a.MessageBody}) {
		return tools.Ok(map[string]any{"sent": 0, "captured": "a user-flow replay is running: the notification was kept for it, not pushed"}), nil
	}
	q := `SELECT endpoint_url, p256dh, auth_key FROM of_push_subscriptions WHERE endpoint_path = ?`
	qArgs := []any{path}
	if a.UserID > 0 {
		q += " AND user_id = ?"
		qArgs = append(qArgs, a.UserID)
	}
	type subscription struct {
		endpointURL string
		p256dh      string
		authKey     string
	}

	rows, err := ctx.DB.Query(q, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing subs: %w", err)
	}
	var collected []subscription
	for rows.Next() {
		var s subscription
		if err := rows.Scan(&s.endpointURL, &s.p256dh, &s.authKey); err != nil {
			continue
		}
		collected = append(collected, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing subs: %w", err)
	}

	payload := []byte(fmt.Sprintf(`{"title":%q,"body":%q,"url":%q}`, a.Title, a.MessageBody, a.URL))
	// SSRF-hardened transport: subscription endpoints come from browser
	// PushSubscriptions and must be treated as untrusted URLs. This client blocks
	// private/internal IPs at connect time and re-validates redirects;
	// SendWebPush also pre-flights each endpoint with netguard.ValidateExternalURL.
	client := netguard.SSRFSafeClient()
	// Send only once the cursor is closed: ctx.DB has MaxOpenConns=1, so the
	// expired-subscription DELETE below would deadlock against an open cursor on
	// the same handle, and an open cursor would hold the project's only write
	// connection across every network round-trip.
	var sent, failed int
	for _, s := range collected {
		gone, perr := SendWebPush(client, s.endpointURL, s.p256dh, s.authKey, payload, pubKey, privKey, contact)
		if perr != nil {
			failed++
			if gone {
				_, _ = ctx.DB.Exec("DELETE FROM of_push_subscriptions WHERE endpoint_url = ?", s.endpointURL)
			}
			continue
		}
		sent++
	}
	return tools.Ok(map[string]any{"sent": sent, "failed": failed}), nil
}

// PushListSubscribers lists active subscriptions for one endpoint or all
// endpoints when endpoint_path is omitted.
type PushListSubscribers struct {
	EndpointPath string `arg:"endpoint_path" desc:"Filter subscribers to a specific endpoint."`
}

func (a *PushListSubscribers) Doc() string {
	return "List active subscriptions for one endpoint, or all when endpoint_path is omitted"
}
func (a *PushListSubscribers) ReadOnly() bool { return true }
func (a *PushListSubscribers) Example() any {
	return &PushListSubscribers{EndpointPath: "news"}
}
func (a *PushListSubscribers) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	q := `SELECT id, user_id, endpoint_url, created_at FROM of_push_subscriptions`
	var qArgs []any
	if path != "" {
		q += " WHERE endpoint_path = ?"
		qArgs = append(qArgs, path)
	}
	q += " ORDER BY id DESC LIMIT 200"
	rows, err := ctx.DB.Query(q, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var userID any
		var url string
		var created time.Time
		if err := rows.Scan(&id, &userID, &url, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "user_id": userID, "endpoint_url": url, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}
