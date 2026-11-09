package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// rowQuerier is the one method LoadValidationRules needs, satisfied by a pool and a
// *sql.Tx alike, so a write inside a transaction can load rules through it.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Per-column validation rules (manage_schema set_validation), stored in
// of_dynamic_tables.validation and enforced on every write path: the build-time data
// tools and the runtime CRUD API, so a rule the model sets actually rejects bad input
// from the app's own forms.

// ValidationError holds a single field validation failure.
type ValidationError struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// formatPatterns contains pre-defined regex patterns for format validation.
var formatPatterns = map[string]*regexp.Regexp{
	"email": regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`),
	"url":   regexp.MustCompile(`^https?://`),
	"phone": regexp.MustCompile(`^\+?[\d\s\-\(\)]{7,20}$`),
	"uuid":  regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
	"date":  regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`),
	"time":  regexp.MustCompile(`^\d{2}:\d{2}(:\d{2})?$`),
}

// LoadValidationRules reads a table's column validation rules from
// of_dynamic_tables, returning nil when none are set. Load once per import and
// reuse the result for every row: it issues a query on ctx.DB and must not run
// while a transaction holds the single-connection writer pool, or it would
// block waiting for a connection the transaction already owns.
func LoadValidationRules(db rowQuerier, tableName string) map[string]map[string]any {
	var rawValidation string
	err := db.QueryRow("SELECT validation FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&rawValidation)
	if err != nil || rawValidation == "" || rawValidation == "{}" {
		return nil
	}

	// The column also stores internal keys (create_vector_index's string-valued
	// _embedding_config), so decode loosely and keep only per-column rule objects:
	// a strict map-of-maps decode failed on that one string and silently dropped
	// every rule on the table.
	var raw map[string]any
	if json.Unmarshal([]byte(rawValidation), &raw) != nil {
		return nil
	}
	rules := map[string]map[string]any{}
	for col, v := range raw {
		if strings.HasPrefix(col, "_") {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			rules[col] = m
		}
	}
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// ValidationMessage joins a row's validation failures into one sentence for an error.
func ValidationMessage(errs []ValidationError) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}

// ValidateRow checks a row against pre-loaded validation rules. It performs no
// database access, so it is safe to call inside a transaction. Returns nil when
// all validations pass.
func ValidateRow(rules map[string]map[string]any, row map[string]any) []ValidationError {
	var errors []ValidationError
	for field, fieldRules := range rules {
		val, exists := row[field]
		if !exists {
			continue // field not in this row, skip (NOT NULL is handled by SQL constraints)
		}

		strVal := fmt.Sprintf("%v", val)
		customMsg, _ := fieldRules["custom_message"].(string)

		// min_length
		if ml, ok := fieldRules["min_length"].(float64); ok {
			if len(strVal) < int(ml) {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s must be at least %d characters", field, int(ml))
				}
				errors = append(errors, ValidationError{Field: field, Rule: "min_length", Message: msg})
			}
		}

		// max_length
		if ml, ok := fieldRules["max_length"].(float64); ok {
			if len(strVal) > int(ml) {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s must be at most %d characters", field, int(ml))
				}
				errors = append(errors, ValidationError{Field: field, Rule: "max_length", Message: msg})
			}
		}

		// min (numeric)
		if minVal, ok := fieldRules["min"].(float64); ok {
			if numVal, ok := toFloat64(val); ok && numVal < minVal {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s must be at least %g", field, minVal)
				}
				errors = append(errors, ValidationError{Field: field, Rule: "min", Message: msg})
			}
		}

		// max (numeric)
		if maxVal, ok := fieldRules["max"].(float64); ok {
			if numVal, ok := toFloat64(val); ok && numVal > maxVal {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s must be at most %g", field, maxVal)
				}
				errors = append(errors, ValidationError{Field: field, Rule: "max", Message: msg})
			}
		}

		// pattern (regex)
		if pattern, ok := fieldRules["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			if err == nil && !re.MatchString(strVal) {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s does not match required pattern", field)
				}
				errors = append(errors, ValidationError{Field: field, Rule: "pattern", Message: msg})
			}
		}

		// format (preset)
		if format, ok := fieldRules["format"].(string); ok {
			if re, exists := formatPatterns[format]; exists {
				if !re.MatchString(strVal) {
					msg := customMsg
					if msg == "" {
						msg = fmt.Sprintf("%s must be a valid %s", field, format)
					}
					errors = append(errors, ValidationError{Field: field, Rule: "format", Message: msg})
				}
			}
		}

		// enum (allowed values)
		if enumVals, ok := fieldRules["enum"].([]any); ok {
			found := false
			for _, ev := range enumVals {
				if fmt.Sprintf("%v", ev) == strVal {
					found = true
					break
				}
			}
			if !found {
				msg := customMsg
				if msg == "" {
					msg = fmt.Sprintf("%s must be one of: %v", field, enumVals)
				}
				errors = append(errors, ValidationError{Field: field, Rule: "enum", Message: msg})
			}
		}
	}

	if len(errors) == 0 {
		return nil
	}
	return errors
}

// toFloat64 converts a value to float64 for numeric comparisons.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		// Try parsing as number.
		var f float64
		if _, err := fmt.Sscanf(n, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
