package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const mcpToolNamePrefix = "mcp__"

// Anthropic's tool definition schema permits 1–128 ASCII name characters:
// https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools
const maxToolNameLength = 128

var customToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// toolNameMapping contains only names changed by the experimental ingress.
// A client that already declared an mcp__ name therefore never has that name
// stripped from the response.
type toolNameMapping map[string]string

// normalizeExperimentalToolNames adapts third-party custom tool names to the
// MCP-shaped names that avoid observed subscription tool-name classification.
// Anthropic server tools carry a versioned type and require their fixed names, so
// they are never renamed. References in tool_choice and prior tool_use blocks are changed
// only when their matching custom declaration was renamed.
func normalizeExperimentalToolNames(body []byte) ([]byte, int, toolNameMapping, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, 0, nil, fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, 0, nil, fmt.Errorf("decode request body: trailing JSON value")
	}
	if root == nil {
		return nil, 0, nil, fmt.Errorf("request body must be a JSON object")
	}

	changed := 0
	renamed := make(map[string]string)
	restored := make(toolNameMapping)
	if tools, ok := root["tools"].([]any); ok {
		var err error
		renamed, err = experimentalToolAliases(tools)
		if err != nil {
			return nil, 0, nil, err
		}
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			normalized, ok := renamed[name]
			if !ok {
				continue
			}
			tool["name"] = normalized
			restored[normalized] = name
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
		return body, 0, nil, nil
	}
	transformed, err := json.Marshal(root)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode tool-normalized request body: %w", err)
	}
	return transformed, changed, restored, nil
}

func experimentalToolAliases(tools []any) (map[string]string, error) {
	// Reserve every original name, including server and already-prefixed tools.
	// A caller can also declare a name that happens to match a generated alias.
	used := make(map[string]struct{}, len(tools))
	var customNames []string
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		if _, duplicate := used[name]; duplicate {
			return nil, fmt.Errorf("duplicate tool name %q", name)
		}
		used[name] = struct{}{}
		// Unknown/versioned types have fixed protocol names. Leave invalid caller
		// names to upstream validation rather than making them valid by truncation.
		if toolType, typed := tool["type"]; typed && toolType != "custom" {
			continue
		}
		if strings.HasPrefix(name, mcpToolNamePrefix) || !customToolNamePattern.MatchString(name) {
			continue
		}
		customNames = append(customNames, name)
	}

	renamed := make(map[string]string, len(customNames))
	var aliasedNames []string
	for _, name := range customNames {
		normalized := mcpToolNamePrefix + name
		if _, collision := used[normalized]; collision || len(normalized) > maxToolNameLength {
			aliasedNames = append(aliasedNames, name)
			continue
		}
		renamed[name] = normalized
		used[normalized] = struct{}{}
	}
	// Allocate exceptional aliases after all ordinary targets are reserved.
	// Sorting makes even alias-collision resolution independent of tool order.
	sort.Strings(aliasedNames)
	for _, name := range aliasedNames {
		sum := sha256.Sum256([]byte(name))
		for attempt := 0; ; attempt++ {
			suffix := fmt.Sprintf("__%x", sum[:8])
			if attempt > 0 {
				suffix += fmt.Sprintf("_%d", attempt)
			}
			head := name[:min(len(name), maxToolNameLength-len(mcpToolNamePrefix)-len(suffix))]
			normalized := mcpToolNamePrefix + head + suffix
			if _, collision := used[normalized]; collision {
				continue
			}
			renamed[name] = normalized
			used[normalized] = struct{}{}
			break
		}
	}
	return renamed, nil
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
