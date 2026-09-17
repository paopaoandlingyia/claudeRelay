package proxy

import (
	"encoding/json"
	"testing"
)

func TestNormalizeCompatibleToolNamesUpdatesDeclarationsAndReferences(t *testing.T) {
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

	transformed, changed, err := normalizeCompatibleToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Fatalf("changed names = %d, want 3", changed)
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

func TestNormalizeCompatibleToolNamesLeavesBuiltInToolAndReferencesUnchanged(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"web_search","input":{}}
		]}]
	}`)
	transformed, changed, err := normalizeCompatibleToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || string(transformed) != string(body) {
		t.Fatalf("changed=%d body=%s", changed, transformed)
	}
}

func TestNormalizeCompatibleToolNamesLeavesAlreadyCompatibleBodyByteExact(t *testing.T) {
	t.Parallel()
	body := []byte("{\n  \"tools\": [{\"name\":\"mcp__web_search\"}], \"messages\": []\n}")
	transformed, changed, err := normalizeCompatibleToolNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || string(transformed) != string(body) {
		t.Fatalf("changed=%d body=%s", changed, transformed)
	}
}
