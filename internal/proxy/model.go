package proxy

import (
	"encoding/json"
	"strings"
)

// extractModel extracts the model name from a JSON request body.
func extractModel(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "application/json") {
		return ""
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	modelValue, ok := payload["model"]
	if !ok {
		return ""
	}
	modelString, ok := modelValue.(string)
	if !ok {
		return ""
	}
	return modelString
}

// replaceModel replaces the model field in a JSON body with a new model name.
func replaceModel(body []byte, to string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	payload["model"] = to
	replaced, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return replaced
}

// modelMetricLabel returns a normalized label for metrics based on the model name.
func modelMetricLabel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case normalized == "":
		return "unknown"
	case strings.HasPrefix(normalized, "claude-opus"):
		return "claude-opus"
	case strings.HasPrefix(normalized, "claude-sonnet"):
		return "claude-sonnet"
	case strings.HasPrefix(normalized, "claude-haiku"):
		return "claude-haiku"
	case strings.HasPrefix(normalized, "claude-"):
		return "claude-other"
	default:
		return "custom"
	}
}
