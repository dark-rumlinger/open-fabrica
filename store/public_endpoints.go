package store

import "database/sql"

// DocumentEndpointConfig holds the configuration for a runtime PDF endpoint.
type DocumentEndpointConfig struct {
	Template     string
	Filename     string
	PageSize     string
	Landscape    bool
	RequiresAuth bool
	RateLimit    int
}

// FunctionEndpointConfig holds the configuration for a custom JS function endpoint.
type FunctionEndpointConfig struct {
	Path         string
	Code         string
	Methods      string
	RequiresAuth bool
	CORSOrigins  string
	RateLimit    int
	TimeoutMS    int
	Secrets      string
	Description  string
}

// LoadDocumentEndpoint loads a document endpoint by path from of_document_endpoints.
func LoadDocumentEndpoint(reader *sql.DB, path string, defaultLimit int) (*DocumentEndpointConfig, error) {
	var ep DocumentEndpointConfig
	err := reader.QueryRow(
		`SELECT template_html, filename, page_size, COALESCE(landscape,0), COALESCE(requires_auth,1), COALESCE(rate_limit,?)
		   FROM of_document_endpoints WHERE path = ?`, defaultLimit, path,
	).Scan(&ep.Template, &ep.Filename, &ep.PageSize, &ep.Landscape, &ep.RequiresAuth, &ep.RateLimit)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// LoadFunctionEndpoint loads a custom function endpoint by path from of_function_endpoints.
func LoadFunctionEndpoint(reader *sql.DB, path string) (*FunctionEndpointConfig, error) {
	var ep FunctionEndpointConfig
	err := reader.QueryRow(
		"SELECT path, code, methods, COALESCE(requires_auth,0), COALESCE(cors_origins,''), COALESCE(rate_limit,60), COALESCE(timeout_ms,0), COALESCE(secrets,''), COALESCE(description,'') FROM of_function_endpoints WHERE path = ?",
		path,
	).Scan(&ep.Path, &ep.Code, &ep.Methods, &ep.RequiresAuth, &ep.CORSOrigins, &ep.RateLimit, &ep.TimeoutMS, &ep.Secrets, &ep.Description)
	if err != nil {
		return nil, err
	}
	if ep.Methods == "" {
		ep.Methods = "POST"
	}
	if ep.RateLimit <= 0 {
		ep.RateLimit = 60
	}
	return &ep, nil
}
