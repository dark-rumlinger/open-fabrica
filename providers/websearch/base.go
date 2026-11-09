package websearch

import (
	"time"

	"openfabrica/providers/internal/providerbase"
)

// BaseClient is the shared provider connection base (see providers/internal/providerbase).
// Web-search adapters embed *BaseClient and supply only Type/Capabilities/Search.
type BaseClient = providerbase.BaseClient

// Defaults carries an adapter's own vendor knowledge (base URL, credential
// placement, static headers) for NewBaseClient to layer under the row's config.
type Defaults = providerbase.Defaults

// NewBaseClient builds a BaseClient with the web-search default timeout of 15s
// when Options.HTTPClient is nil (search APIs can be slower than a CDN image URL).
func NewBaseClient(o Options, def Defaults) *BaseClient {
	return providerbase.NewBaseClient(o, def, 15*time.Second)
}
