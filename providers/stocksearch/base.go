package stocksearch

import (
	"time"

	"openfabrica/providers/internal/providerbase"
)

// BaseClient is the shared provider connection base (see providers/internal/providerbase).
// Stock-search adapters embed *BaseClient and supply only Type/Capabilities/Search.
type BaseClient = providerbase.BaseClient

// Defaults carries an adapter's own vendor knowledge (base URL, credential
// placement, static headers) for NewBaseClient to layer under the row's config.
type Defaults = providerbase.Defaults

// NewBaseClient builds a BaseClient with the stock-search default timeout of 10s
// when Options.HTTPClient is nil.
func NewBaseClient(o Options, def Defaults) *BaseClient {
	return providerbase.NewBaseClient(o, def, 10*time.Second)
}
