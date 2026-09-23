package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRestoreNonStreamingToolNamesUsesRequestMapping(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_1",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"mcp__weather","input":{}},
			{"type":"text","text":"mcp__weather remains text"},
			{"type":"tool_use","id":"toolu_2","name":"mcp__client_owned","input":{}}
		],
		"usage":{"input_tokens":1,"output_tokens":2}
	}`)

	transformed, changed, err := restoreNonStreamingToolNames(body, toolNameMapping{"mcp__weather": "weather"})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed names = %d, want 1", changed)
	}
	var root map[string]any
	if err := json.Unmarshal(transformed, &root); err != nil {
		t.Fatal(err)
	}
	content := root["content"].([]any)
	if got := content[0].(map[string]any)["name"]; got != "weather" {
		t.Fatalf("mapped tool name = %q", got)
	}
	if got := content[1].(map[string]any)["text"]; got != "mcp__weather remains text" {
		t.Fatalf("text block changed = %q", got)
	}
	if got := content[2].(map[string]any)["name"]; got != "mcp__client_owned" {
		t.Fatalf("unmapped tool name changed = %q", got)
	}
}

func TestRestoreNonStreamingToolNamesKeepsUnmatchedBodyByteExact(t *testing.T) {
	t.Parallel()
	body := []byte("{\n  \"content\": [{\"type\":\"text\",\"text\":\"ok\"}]\n}")
	transformed, changed, err := restoreNonStreamingToolNames(body, toolNameMapping{"mcp__weather": "weather"})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || !bytes.Equal(transformed, body) {
		t.Fatalf("changed=%d body=%s", changed, transformed)
	}
}

func TestCopySSEWithRestoredToolNamesRewritesOnlyToolStart(t *testing.T) {
	t.Parallel()
	stream := "event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"mcp__weather\",\"input\":{}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"mcp__weather\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var output bytes.Buffer
	written, changed, err := copySSEWithRestoredToolNames(&output, strings.NewReader(stream), toolNameMapping{"mcp__weather": "weather"})
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(output.Len()) || changed != 1 {
		t.Fatalf("written=%d output=%d changed=%d", written, output.Len(), changed)
	}
	if !strings.Contains(output.String(), `"name":"weather"`) {
		t.Fatalf("tool start was not restored: %s", output.String())
	}
	if !strings.Contains(output.String(), `"partial_json":"mcp__weather"`) {
		t.Fatalf("tool input delta changed: %s", output.String())
	}
}

func TestRestoreSSEEventToolNameRejectsMalformedToolStart(t *testing.T) {
	t.Parallel()
	event := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\"\n\n")
	if _, _, err := restoreSSEEventToolName(event, toolNameMapping{"mcp__weather": "weather"}); err == nil {
		t.Fatal("malformed content_block_start was accepted")
	}
}
