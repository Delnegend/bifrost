package lib

import "testing"

// TestValidateConfigSchema_LogsStoreObjectStorageRoutes checks that a route
// with a guardrail match and its own bucket validates, and that an empty
// match, an unknown outcome or a missing bucket is rejected.
func TestValidateConfigSchema_LogsStoreObjectStorageRoutes(t *testing.T) {
	const valid = `{
		"logs_store": {
			"enabled": true,
			"type": "sqlite",
			"config": {"path": "./logs.db"},
			"object_storage": {"type": "s3", "bucket": "logs"},
			"object_storage_routes": [
				{
					"name": "guardrail-flagged",
					"match": {"guardrail_outcomes": ["blocked", "redacted"], "guardrail_phases": ["input", "output"]},
					"object_storage": {"type": "s3", "bucket": "quarantine", "format": "parquet"}
				},
				{
					"name": "rate-limits",
					"match": {"error_types": ["rate_limit_error"], "status_codes": [429]},
					"object_storage": {"type": "gcs", "bucket": "errors"}
				}
			]
		}
	}`
	if err := ValidateConfigSchema([]byte(valid), loadLocalSchema(t)); err != nil {
		t.Fatalf("expected routes to validate, got: %v", err)
	}

	invalid := map[string]string{
		"empty match":     `{"name": "r", "match": {}, "object_storage": {"type": "s3", "bucket": "b"}}`,
		"unknown outcome": `{"name": "r", "match": {"guardrail_outcomes": ["block"]}, "object_storage": {"type": "s3", "bucket": "b"}}`,
		"unknown field":   `{"name": "r", "match": {"guardrail_outcome": ["blocked"]}, "object_storage": {"type": "s3", "bucket": "b"}}`,
		"missing bucket":  `{"name": "r", "match": {"guardrail_outcomes": ["blocked"]}}`,
		"bad name":        `{"name": "Bad Name", "match": {"guardrail_outcomes": ["blocked"]}, "object_storage": {"type": "s3", "bucket": "b"}}`,
	}
	for name, route := range invalid {
		t.Run(name, func(t *testing.T) {
			config := `{"logs_store": {"enabled": true, "type": "sqlite", "config": {"path": "./logs.db"}, "object_storage_routes": [` + route + `]}}`
			if err := ValidateConfigSchema([]byte(config), loadLocalSchema(t)); err == nil {
				t.Fatal("expected the route to be rejected")
			}
		})
	}
}
