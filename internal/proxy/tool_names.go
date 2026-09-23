package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const mcpToolNamePrefix = "mcp__"

// normalizeExperimentalToolNames adapts third-party custom tool names to the
// MCP-shaped names required by the subscription upstream. Anthropic server
// tools carry a versioned type and require their fixed names, so they are never
// renamed. References in tool_choice and prior tool_use blocks are changed
// only when their matching custom declaration was renamed.
func normalizeExperimentalToolNames(body []byte) ([]byte, int, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, 0, fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, 0, fmt.Errorf("decode request body: trailing JSON value")
	}
	if root == nil {
		return nil, 0, fmt.Errorf("request body must be a JSON object")
	}

	changed := 0
	renamed := make(map[string]string)
	if tools, ok := root["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Built-in tools such as web_search_20250305 are discriminated by
			// type and reject any name other than their documented fixed name.
			if _, builtIn := tool["type"]; builtIn {
				continue
			}
			name, ok := tool["name"].(string)
			if !ok || name == "" || strings.HasPrefix(name, mcpToolNamePrefix) {
				continue
			}
			normalized := mcpToolNamePrefix + name
			tool["name"] = normalized
			renamed[name] = normalized
			changed++
		}
	}
	if toolChoice, ok := root["tool_choice"].(map[string]any); ok && renameToolReference(toolChoice, renamed) {
		changed++
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			content, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok || block["type"] != "tool_use" {
					continue
				}
				if renameToolReference(block, renamed) {
					changed++
				}
			}
		}
	}
	if changed == 0 {
		return body, 0, nil
	}
	transformed, err := json.Marshal(root)
	if err != nil {
		return nil, 0, fmt.Errorf("encode tool-normalized request body: %w", err)
	}
	return transformed, changed, nil
}

func renameToolReference(value map[string]any, renamed map[string]string) bool {
	name, ok := value["name"].(string)
	if !ok {
		return false
	}
	normalized, ok := renamed[name]
	if !ok {
		return false
	}
	value["name"] = normalized
	return true
}
