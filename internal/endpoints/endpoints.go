// Package endpoints names the facts about a project's public endpoints that the
// tools which create them and the public server which serves them must agree on:
// the kinds recorded in the of_endpoints spine and the upload size ceilings. A
// leaf package, so both sides import it without the server depending on a tool.
package endpoints

import "strings"

// Endpoint kinds recorded in the of_endpoints spine - the single registry of
// what is mounted on the public /api/<path> namespace. Feeds (/feed/<path>) and
// webhooks (/webhooks/..) live in separate URL namespaces and are NOT recorded.
// Stored values: never rename one without a migration.
const (
	KindCRUD     = "crud"
	KindFunction = "function"
	KindQuery    = "query"
	KindAuth     = "auth"
	KindUpload   = "upload"
	KindSSE      = "sse"
	KindLLM      = "llm"
	KindAgent    = "agent"
	KindWS       = "ws"
	KindExport   = "export"
	KindImport   = "import"
	KindTOTP     = "totp"
	KindMagic    = "magic"
	KindPush     = "push"
	KindDocument = "document"
)

// MaxUploadSizeMB is the absolute upper bound for any upload endpoint, no
// matter what the model passes in. manage_uploads clamps configured sizes to it
// and the public server enforces it again at request time as defense-in-depth
// against stale config rows.
const MaxUploadSizeMB = 500

// MaxProjectUploadBytes caps total user-uploaded bytes per project so an
// internet visitor cannot fill the host disk by spamming an upload endpoint.
// Enforced by the public server against the sum of stored upload sizes.
const MaxProjectUploadBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// AuthModel derives the auth_model a stored REST endpoint has from the columns
// manage_api writes (the inverse of its auth_model translation): authored scope wins,
// then an owner column, a required role, a token requirement without public_read,
// else public. A public_read endpoint is world-readable (only its writes need a
// token). One home so the conformance validator, the RLS validator and manage_api's
// own listing all read an endpoint's model the same way.
func AuthModel(requiresAuth, publicRead bool, requiredRole, ownerColumn, ownerScope string) string {
	switch {
	case ownerColumn != "" && strings.EqualFold(ownerScope, "authored"):
		return "authored"
	case ownerColumn != "":
		return "owner"
	case requiredRole != "":
		return "role"
	case requiresAuth && !publicRead:
		return "authenticated"
	}
	return "public"
}

// PlatformRoutePrefixes are the URL namespaces the public server owns outright:
// everything under one is served by the platform (an API, an asset, a feed, a webhook
// receiver, the notification inbox), never by a saved page. A link into one is not a
// page route, so link checks must not resolve it against the page table. The public
// server's route table is held to this list by a test, so a new namespace cannot be
// mounted without being named here.
var PlatformRoutePrefixes = []string{"/api/", "/files/", "/feed/", "/webhooks/", "/payments/", "/__of/"}

// IsPlatformRoute reports whether path lies under a platform-owned namespace.
func IsPlatformRoute(path string) bool {
	for _, p := range PlatformRoutePrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
