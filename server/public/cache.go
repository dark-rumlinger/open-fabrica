package public

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"openfabrica/internal/safego"
)

// apiCache is a simple in-memory cache for API GET responses.
// Keyed by "projectID:path:queryString", entries expire after a configurable TTL.
type apiCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	body      []byte
	status    int
	expiresAt time.Time
}

func newAPICache() *apiCache {
	c := &apiCache{entries: make(map[string]*cacheEntry)}
	// Background cleanup every 60 seconds.
	safego.Go("api-cache-evict", func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			c.evict()
		}
	})
	return c
}

// get returns a cached response if it exists and hasn't expired.
func (c *apiCache) get(key string) ([]byte, int, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || time.Now().After(entry.expiresAt) {
		return nil, 0, false
	}
	return entry.body, entry.status, true
}

// set stores a response in the cache with the given TTL.
func (c *apiCache) set(key string, body []byte, status int, ttl time.Duration) {
	c.mu.Lock()
	c.entries[key] = &cacheEntry{
		body:      body,
		status:    status,
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Unlock()
}

// invalidateTable removes all cache entries for a specific project+table combination.
// Called on POST/PUT/DELETE to ensure stale data isn't served.
func (c *apiCache) invalidateTable(projectID int, tableName string) {
	prefix := formatInvalidationPrefix(projectID, tableName)
	c.mu.Lock()
	for key := range c.entries {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}

// evict removes all expired entries.
func (c *apiCache) evict() {
	now := time.Now()
	c.mu.Lock()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}

// formatCacheKey creates a cache key from project ID, table, the caller's row-level
// scope, path, and query string. ownerVal/tenantVal are the owner/tenant filter values
// applied to the query (nil when unscoped/public); folding them in ensures a response
// computed under one caller's scope is never replayed to a different caller - the
// cross-user/cross-tenant disclosure a scope-blind key would cause. Unscoped responses
// (both nil) share one identity-free entry. The invalidation prefix
// (projectID:tableName:) is preserved so invalidateTable still evicts every per-caller
// variant on a write.
func formatCacheKey(projectID int, tableName string, ownerVal, tenantVal any, path, query string) string {
	return formatInvalidationPrefix(projectID, tableName) +
		"o" + scopeKeyPart(ownerVal) + ":t" + scopeKeyPart(tenantVal) + ":" +
		path + "?" + query
}

// scopeKeyPart renders an owner/tenant filter value for the cache key. A nil
// value (no filter applied) renders empty, so unscoped responses share one key.
func scopeKeyPart(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// formatInvalidationPrefix creates the prefix used for table-level invalidation.
func formatInvalidationPrefix(projectID int, tableName string) string {
	return fmt.Sprintf("%d:%s:", projectID, tableName)
}

// responseRecorder captures HTTP responses for caching while writing through to the client.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return r.ResponseWriter.Write(b)
}
