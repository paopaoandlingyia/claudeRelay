package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeExperimentalToolNamesUpdatesDeclarationsAndReferences(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"model":"claude-test",
		"tools":[
			{"name":"web_search","description":"search","input_schema":{"type":"object"}},
			{"name":"mcp__already_prefixed","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"web_search","input":{"q":"test"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}
		]
	}`)

	transformed, changed, names, err := normalizeExperimentalToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Fatalf("changed names = %d, want 3", changed)
	}
	if len(names) != 1 || names["mcp__web_search"] != "web_search" {
		t.Fatalf("response name mapping = %#v", names)
	}
	var root map[string]any
	if err := json.Unmarshal(transformed, &root); err != nil {
		t.Fatal(err)
	}
	tools := root["tools"].([]any)
	if got := tools[0].(map[string]any)["name"]; got != "mcp__web_search" {
		t.Fatalf("tool name = %q", got)
	}
	if got := tools[1].(map[string]any)["name"]; got != "mcp__already_prefixed" {
		t.Fatalf("prefixed tool name = %q", got)
	}
	if got := root["tool_choice"].(map[string]any)["name"]; got != "mcp__web_search" {
		t.Fatalf("tool choice name = %q", got)
	}
	messages := root["messages"].([]any)
	toolUse := messages[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if got := toolUse["name"]; got != "mcp__web_search" {
		t.Fatalf("tool use name = %q", got)
	}
}

func TestNormalizeExperimentalToolNamesLeavesBuiltInToolAndReferencesUnchanged(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"web_search","input":{}}
		]}]
	}`)
	transformed, changed, names, err := normalizeExperimentalToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || len(names) != 0 || string(transformed) != string(body) {
		t.Fatalf("changed=%d names=%#v body=%s", changed, names, transformed)
	}
}

func TestNormalizeExperimentalToolNamesLeavesAlreadyCompatibleBodyByteExact(t *testing.T) {
	t.Parallel()
	body := []byte("{\n  \"tools\": [{\"name\":\"mcp__web_search\"}], \"messages\": []\n}")
	transformed, changed, names, err := normalizeExperimentalToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || len(names) != 0 || string(transformed) != string(body) {
		t.Fatalf("changed=%d names=%#v body=%s", changed, names, transformed)
	}
}

func TestNormalizeExperimentalToolNamesResolvesCollisionAndRestoresResponses(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"system":"Only output JSON.",
		"tools":[{"name":"search","type":"custom"},{"name":"mcp__search"}],
		"tool_choice":{"type":"tool","name":"search"},
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","name":"search","input":{"name":"search"}},
			{"type":"tool_use","name":"mcp__search","input":{}}
		]}]
	}`)
	transformed, changed, names, err := normalizeExperimentalToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	root := decodeBody(t, transformed)
	tools := root["tools"].([]any)
	alias := tools[0].(map[string]any)["name"].(string)
	if !strings.HasPrefix(alias, "mcp__search__") || alias == "mcp__search" || names[alias] != "search" || len(names) != 1 || changed != 3 {
		t.Fatalf("alias=%q changed=%d mapping=%v", alias, changed, names)
	}
	if root["system"] != "Only output JSON." || tools[1].(map[string]any)["name"] != "mcp__search" || root["tool_choice"].(map[string]any)["name"] != alias {
		t.Fatalf("unrelated content or tool_choice changed incorrectly: %s", transformed)
	}
	content := root["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["name"] != alias || content[0].(map[string]any)["input"].(map[string]any)["name"] != "search" || content[1].(map[string]any)["name"] != "mcp__search" {
		t.Fatalf("historical tool references or inputs changed incorrectly: %s", transformed)
	}

	response, _ := json.Marshal(map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": alias, "input": map[string]any{"name": alias}},
		map[string]any{"type": "tool_use", "name": "mcp__search", "input": map[string]any{}},
		map[string]any{"type": "text", "text": alias},
	}})
	restored, count, err := restoreNonStreamingToolNames(response, names)
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeBody(t, restored)["content"].([]any)
	if count != 1 || blocks[0].(map[string]any)["name"] != "search" || blocks[1].(map[string]any)["name"] != "mcp__search" || blocks[2].(map[string]any)["text"] != alias || blocks[0].(map[string]any)["input"].(map[string]any)["name"] != alias {
		t.Fatalf("response restoration changed unrelated fields: %s", restored)
	}
	start := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"name\":\"" + alias + "\",\"input\":{}}}\n\n"
	delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"" + alias + "\"}}\n\n"
	var output bytes.Buffer
	if _, count, err := copySSEWithRestoredToolNames(&output, strings.NewReader(start+delta), names); err != nil || count != 1 {
		t.Fatalf("SSE restoration count=%d err=%v", count, err)
	}
	if !strings.Contains(output.String(), `"name":"search"`) || !strings.HasSuffix(output.String(), delta) {
		t.Fatalf("SSE alias or input delta changed incorrectly: %s", output.String())
	}
}

func TestExperimentalToolAliasesAvoidReservedAndOrdinaryTargets(t *testing.T) {
	t.Parallel()
	base := []any{map[string]any{"name": "search"}, map[string]any{"name": "mcp__search"}}
	initial, err := experimentalToolAliases(base)
	if err != nil {
		t.Fatal(err)
	}
	initialAlias := initial["search"]
	for _, tc := range []struct {
		name  string
		extra []any
	}{
		{"declared aliases", []any{map[string]any{"name": initialAlias}, map[string]any{"name": initialAlias + "_1"}}},
		{"ordinary target", []any{map[string]any{"name": strings.TrimPrefix(initialAlias, mcpToolNamePrefix)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := append(append([]any{}, base...), tc.extra...)
			aliases, err := experimentalToolAliases(tools)
			if err != nil {
				t.Fatal(err)
			}
			if aliases["search"] == initialAlias {
				t.Fatalf("generated alias consumed a reserved target: %v", aliases)
			}
			used := make(map[string]bool)
			for _, raw := range tools {
				original := raw.(map[string]any)["name"].(string)
				target := original
				if alias, ok := aliases[original]; ok {
					target = alias
				}
				if used[target] {
					t.Fatalf("duplicate target %q in %v", target, aliases)
				}
				used[target] = true
			}
			for i, j := 0, len(tools)-1; i < j; i, j = i+1, j-1 {
				tools[i], tools[j] = tools[j], tools[i]
			}
			reordered, err := experimentalToolAliases(tools)
			if err != nil || !reflect.DeepEqual(aliases, reordered) {
				t.Fatalf("mapping depends on declaration order: %v vs %v (err=%v)", aliases, reordered, err)
			}
		})
	}
}

func TestExperimentalToolAliasesBoundLengthWithoutMergingLongNames(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("a", maxToolNameLength-len(mcpToolNamePrefix))
	long := strings.Repeat("a", maxToolNameLength-1)
	names := []string{short, short + "b", long + "b", long + "c"}
	var tools []any
	for _, name := range names {
		tools = append(tools, map[string]any{"name": name})
	}
	aliases, err := experimentalToolAliases(tools)
	if err != nil {
		t.Fatal(err)
	}
	used := make(map[string]bool)
	for _, name := range names {
		alias := aliases[name]
		if !strings.HasPrefix(alias, mcpToolNamePrefix) || len(alias) > maxToolNameLength || !customToolNamePattern.MatchString(alias) || used[alias] {
			t.Fatalf("invalid or duplicate alias %q for %q", alias, name)
		}
		used[alias] = true
	}
	if aliases[short] != mcpToolNamePrefix+short {
		t.Fatalf("ordinary maximum-length target changed: %q", aliases[short])
	}
	for _, name := range names[1:] {
		single, err := experimentalToolAliases([]any{map[string]any{"name": name}})
		if err != nil || aliases[name] != single[name] {
			t.Fatalf("alias depends on unrelated tools: %q vs %q (err=%v)", aliases[name], single[name], err)
		}
	}
}

func TestExperimentalToolAliasesPreservePrefixedTypedAndInvalidNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mcp__search", "mcp__" + strings.Repeat("x", maxToolNameLength-len(mcpToolNamePrefix)), strings.Repeat("x", maxToolNameLength+1), strings.Repeat("x", maxToolNameLength-1) + "!", "天气"} {
		body, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": name}}})
		transformed, changed, names, err := normalizeExperimentalToolNames(body)
		if err != nil || changed != 0 || len(names) != 0 || !bytes.Equal(body, transformed) {
			t.Fatalf("caller-owned or invalid name %q changed: %s (err=%v)", name, transformed, err)
		}
	}
	aliases, err := experimentalToolAliases([]any{
		map[string]any{"name": "search"},
		map[string]any{"name": "mcp__search", "type": "unknown_server_tool"},
		map[string]any{"name": "web_search", "type": "web_search_20250305"},
	})
	if err != nil || len(aliases) != 1 || aliases["search"] == "mcp__search" {
		t.Fatalf("server names not reserved/preserved: %v (err=%v)", aliases, err)
	}
}

func TestNormalizeExperimentalToolNamesRejectsDuplicateOriginalNames(t *testing.T) {
	t.Parallel()
	body := []byte(`{"tools":[{"name":"search"},{"name":"search"}]}`)
	if _, _, _, err := normalizeExperimentalToolNames(body); err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("duplicate originals must not receive distinct aliases: %v", err)
	}
}
