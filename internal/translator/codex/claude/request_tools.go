package claude

import (
	"strconv"
	"strings"
)

func convertTools(root map[string]any, toolShortNameMap map[string]string) []any {
	toolsRaw := asSlice(root["tools"])
	if len(toolsRaw) == 0 {
		return nil
	}

	out := make([]any, 0, len(toolsRaw))
	for _, rawTool := range toolsRaw {
		tool := asMap(rawTool)
		if asString(tool["type"]) == "web_search_20250305" {
			out = append(out, map[string]any{"type": "web_search"})
			continue
		}

		name := asString(tool["name"])
		if name == "" {
			continue
		}
		if short, ok := toolShortNameMap[name]; ok {
			name = short
		} else {
			name = shortenToolNameIfNeeded(name)
		}

		fn := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": normalizeToolParameters(tool["input_schema"]),
			"strict":     false,
		}
		if desc := asString(tool["description"]); desc != "" {
			fn["description"] = desc
		}
		out = append(out, fn)
	}
	return out
}

func normalizeToolParameters(v any) map[string]any {
	schema := asMapCopy(v)
	if len(schema) == 0 {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	if asString(schema["type"]) == "" {
		schema["type"] = "object"
	}
	if asString(schema["type"]) == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	delete(schema, "$schema")
	return schema
}

func reasoningEffort(root map[string]any) string {
	effort := "medium"

	thinking := asMap(root["thinking"])
	if len(thinking) > 0 {
		switch strings.ToLower(asString(thinking["type"])) {
		case "disabled":
			effort = "minimal"
		case "adaptive", "auto":
			if outputCfg := asMap(root["output_config"]); len(outputCfg) > 0 {
				if v := strings.ToLower(strings.TrimSpace(asString(outputCfg["effort"]))); v != "" {
					return v
				}
			}
			effort = "high"
		case "enabled":
			budget := asInt64(thinking["budget_tokens"])
			switch {
			case budget <= 0:
				effort = "minimal"
			case budget < 2048:
				effort = "low"
			case budget < 8192:
				effort = "medium"
			default:
				effort = "high"
			}
		}
	}

	return effort
}

func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh":
		return effort
	case "minimal", "none", "":
		return "low"
	default:
		return "low"
	}
}

func shouldIncludeReasoningSummary(root map[string]any) bool {
	thinking := asMap(root["thinking"])
	if len(thinking) == 0 {
		return false
	}

	switch strings.ToLower(asString(thinking["type"])) {
	case "enabled", "adaptive", "auto":
		return true
	default:
		return false
	}
}

func buildToolShortNameMap(root map[string]any) map[string]string {
	const toolNameLimit = 64
	names := make([]string, 0)
	for _, raw := range asSlice(root["tools"]) {
		name := asString(asMap(raw)["name"])
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return map[string]string{}
	}

	used := map[string]struct{}{}
	out := make(map[string]string, len(names))
	for _, name := range names {
		candidate := shortenToolNameToLimit(name, toolNameLimit)
		if _, ok := used[candidate]; ok {
			base := candidate
			for i := 1; ; i++ {
				suffix := "_" + strconv.Itoa(i)
				allowed := toolNameLimit - len(suffix)
				if allowed < 0 {
					allowed = 0
				}
				candidate = base
				if len(candidate) > allowed {
					candidate = candidate[:allowed]
				}
				candidate += suffix
				if _, exists := used[candidate]; !exists {
					break
				}
			}
		}
		used[candidate] = struct{}{}
		out[name] = candidate
	}
	return out
}

func shortenToolNameIfNeeded(name string) string {
	return shortenToolNameToLimit(name, 64)
}

func shortenToolNameToLimit(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if idx := strings.LastIndex(name, "__"); idx > 0 && idx+2 < len(name) {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) <= limit {
				return candidate
			}
			return candidate[:limit]
		}
	}
	return name[:limit]
}
