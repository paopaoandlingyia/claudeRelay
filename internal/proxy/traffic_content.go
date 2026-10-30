package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Category byte counts partition the original JSON representation. Container
// punctuation and unknown fields remain in other_json_bytes, never in two categories.
func (o *trafficObservation) describeBody(body []byte, accountID int64, a *trafficAttempt) {
	var request struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		a.counts["body_parse_errors"] = 1
		return
	}
	// Only the bounded model label and numeric counts leave this function.
	a.key.Model = request.Model
	if len(a.key.Model) > 128 {
		a.key.Model = strings.Clone(a.key.Model[:128])
	}
	a.counts["system_bytes"] = int64(len(request.System))
	a.counts["tool_definitions_bytes"] = int64(len(request.Tools))
	for _, message := range request.Messages {
		o.describeContent(message.Content, false, accountID, a.counts)
	}
	categorized := int64(0)
	for _, key := range []string{"system_bytes", "tool_definitions_bytes", "message_text_bytes", "tool_result_text_bytes", "tool_call_bytes", "thinking_bytes", "image_json_bytes", "document_json_bytes"} {
		categorized += a.counts[key]
	}
	a.counts["other_json_bytes"] = int64(len(body)) - categorized
	// Approximately 1% of attempts, at most one sample every ten seconds and
	// at most 4 MiB. No extra compression at all for non-gzip requests.
	if a.counts["request_gzip"] == 0 || len(body) > 4<<20 || o.sequence.Add(1)%100 != 1 {
		return
	}
	o.mu.Lock()
	now := time.Now()
	sample := !now.Before(o.nextSample)
	if sample {
		o.nextSample = now.Add(10 * time.Second)
	}
	o.mu.Unlock()
	if sample {
		sampleContentCompression(body, a.counts)
	}
}

func (o *trafficObservation) describeContent(raw json.RawMessage, toolResult bool, accountID int64, counts map[string]int64) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	if raw[0] == '"' {
		category := "message_text_bytes"
		if toolResult {
			category = "tool_result_text_bytes"
		}
		counts[category] += int64(len(raw))
		return
	}
	if raw[0] != '[' {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, block := range blocks {
		var fields struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Source  struct {
				Type      string `json:"type"`
				Data      string `json:"data"`
				MediaType string `json:"media_type"`
			} `json:"source"`
		}
		if json.Unmarshal(block, &fields) != nil {
			continue
		}
		switch fields.Type {
		case "image":
			counts["images"]++
			counts["image_json_bytes"] += int64(len(block))
			if fields.Source.Type == "base64" {
				counts["inline_images"]++
				counts["image_base64_bytes"] += int64(len(fields.Source.Data))
				n, repeated := o.repeatedImage(fields.Source.Data, accountID)
				counts["image_binary_bytes"] += n
				if repeated {
					counts["repeated_images"]++
					counts["repeated_image_binary_bytes"] += n
				}
				format := "other"
				switch fields.Source.MediaType {
				case "image/png":
					format = "png"
				case "image/jpeg":
					format = "jpeg"
				case "image/webp":
					format = "webp"
				case "image/gif":
					format = "gif"
				}
				counts["image_"+format+"_binary_bytes"] += n
			}
		case "document":
			counts["documents"]++
			counts["document_json_bytes"] += int64(len(block))
		case "text":
			category := "message_text_bytes"
			if toolResult {
				category = "tool_result_text_bytes"
			}
			counts[category] += int64(len(block))
		case "tool_result":
			o.describeContent(fields.Content, true, accountID, counts)
		case "tool_use", "server_tool_use":
			counts["tool_call_bytes"] += int64(len(block))
		case "thinking", "redacted_thinking":
			counts["thinking_bytes"] += int64(len(block))
		}
	}
}

// Marshal both sides in the same canonical key order, so estimates do not
// confuse JSON reordering with compression savings. Samples are marginal
// contributions, not additive components of an exact compressed-byte partition.
func sampleContentCompression(body []byte, counts map[string]int64) {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return
	}
	full, err := json.Marshal(request)
	if err != nil {
		return
	}
	baseline, _, err := encodeUpstreamBody(full, false)
	if err != nil {
		return
	}
	counts["compression_samples"] = 1
	counts["sample_baseline_bytes"] = int64(len(baseline))
	for _, kind := range []string{"image", "document"} {
		replacements := removeInlinePayloads(request["messages"], kind)
		if len(replacements) == 0 {
			counts["sample_"+kind+"_marginal_bytes"] = 0
			continue
		}
		stripped, err := json.Marshal(request)
		for _, replacement := range replacements {
			replacement.source["data"] = replacement.data
		}
		if err != nil {
			continue
		}
		encoded, _, err := encodeUpstreamBody(stripped, false)
		if err != nil {
			continue
		}
		counts["sample_"+kind+"_marginal_bytes"] = int64(len(baseline) - len(encoded))
	}
}

type trafficPayloadReplacement struct {
	source map[string]any
	data   string
}

func removeInlinePayloads(value any, kind string) []trafficPayloadReplacement {
	var replacements []trafficPayloadReplacement
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			replacements = append(replacements, removeInlinePayloads(child, kind)...)
		}
	case map[string]any:
		if value["type"] == kind {
			if source, ok := value["source"].(map[string]any); ok && source["type"] == "base64" {
				if data, ok := source["data"].(string); ok {
					replacements = append(replacements, trafficPayloadReplacement{source, data})
					source["data"] = ""
				}
			}
			return replacements
		}
		// Traverse API content containers only; arbitrary tool input JSON may contain
		// fields named type/source and must not be mistaken for real image blocks.
		if content, ok := value["content"]; ok {
			replacements = append(replacements, removeInlinePayloads(content, kind)...)
		}
	}
	return replacements
}
