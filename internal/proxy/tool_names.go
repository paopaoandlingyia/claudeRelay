package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const mcpToolNamePrefix = "mcp__"

// normalizeCompatibleToolNames adapts ordinary Anthropic tool names to the
// MCP-shaped names required by the subscription upstream. It is called only
// for the compatible ingress. References in tool_choice and prior tool_use
// blocks must be changed together with the declarations or a continued
// conversation would refer to a different tool.
func normalizeCompatibleToolNames(body []byte) ([]byte, int, error) {
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
	if tools, ok := root["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if prefixToolName(tool) {
				changed++
			}
		}
	}
	if toolChoice, ok := root["tool_choice"].(map[string]any); ok && prefixToolName(toolChoice) {
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
				if prefixToolName(block) {
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

func prefixToolName(value map[string]any) bool {
	name, ok := value["name"].(string)
	if !ok || name == "" || strings.HasPrefix(name, mcpToolNamePrefix) {
		return false
	}
	value["name"] = mcpToolNamePrefix + name
	return true
}
