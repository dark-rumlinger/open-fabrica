package public

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/billing"
	"openfabrica/internal/endpoints"
)

// Every route the public server mounts is either a page catch-all, a static file with
// an extension, or lies under endpoints.PlatformRoutePrefixes. The internal-link
// validator exempts exactly that list from page-route resolution, so a namespace
// mounted here but missing from it would make every link into it a false dead_link.
func TestPublicRoutesAreNamedPlatformPrefixes(t *testing.T) {
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{})

	var walked int
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		walked++
		route = strings.TrimSuffix(route, "/*")
		if route == "" || endpoints.IsPlatformRoute(route+"/") {
			return nil
		}
		last := route[strings.LastIndex(route, "/")+1:]
		if strings.Contains(last, ".") {
			return nil // sitemap.xml, robots.txt, manifest.json, favicon.*
		}
		t.Errorf("%s %s is not under endpoints.PlatformRoutePrefixes; add its namespace there", method, route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked == 0 {
		t.Fatal("no routes walked")
	}
	if !endpoints.IsPlatformRoute(billing.WebhookPath) {
		t.Errorf("billing.WebhookPath %q is not a platform route", billing.WebhookPath)
	}
}
