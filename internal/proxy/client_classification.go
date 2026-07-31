package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

const clientClassificationVersion = 2

const (
	clientClassCompatible  = "compatible"
	clientClassCCCandidate = "cc_candidate"
	clientClassAmbiguous   = "ambiguous"
)

type clientEvidence struct {
	BillingBlock       bool
	CCVersion          bool
	KnownEntrypoint    bool
	CCH                bool
	StructuredMetadata bool
	ClaudeUserAgent    bool
	ClaudeCodeSession  bool
	XAppCLI            bool
}

type clientObservation struct {
	Class    string
	Version  int
	Evidence clientEvidence
}

func classifyClient(root map[string]any, headers http.Header) clientObservation {
	evidence, completeBilling := billingEvidence(root["system"])
	evidence.StructuredMetadata = hasStructuredMetadata(root["metadata"])
	userAgent := strings.ToLower(strings.TrimSpace(headers.Get("User-Agent")))
	evidence.ClaudeUserAgent = strings.Contains(userAgent, "claude-cli/")
	evidence.ClaudeCodeSession = strings.TrimSpace(headers.Get(claudeCodeSessionHeader)) != ""
	evidence.XAppCLI = strings.EqualFold(strings.TrimSpace(headers.Get("X-App")), "cli")

	class := clientClassCompatible
	completeHeaders := evidence.ClaudeUserAgent && evidence.ClaudeCodeSession && evidence.XAppCLI
	if completeHeaders || (completeBilling && evidence.StructuredMetadata) {
		class = clientClassCCCandidate
	} else if evidence.BillingBlock || evidence.CCVersion || evidence.KnownEntrypoint ||
		evidence.CCH || evidence.StructuredMetadata || evidence.ClaudeUserAgent ||
		evidence.ClaudeCodeSession || evidence.XAppCLI {
		class = clientClassAmbiguous
	}
	return clientObservation{Class: class, Version: clientClassificationVersion, Evidence: evidence}
}

func billingEvidence(value any) (clientEvidence, bool) {
	var evidence clientEvidence
	complete := false
	for _, text := range systemTexts(value) {
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, billingAttributionPrefix) {
			continue
		}
		evidence.BillingBlock = true
		fields := parseBillingFields(strings.TrimSpace(strings.TrimPrefix(trimmed, billingAttributionPrefix)))
		hasVersion := strings.TrimSpace(fields["cc_version"]) != ""
		entrypoint := strings.ToLower(strings.TrimSpace(fields["cc_entrypoint"]))
		knownEntrypoint := entrypoint == "cli" || entrypoint == "claude-desktop" || entrypoint == "claude-desktop-3p"
		hasCCH := strings.TrimSpace(fields["cch"]) != ""
		evidence.CCVersion = evidence.CCVersion || hasVersion
		evidence.KnownEntrypoint = evidence.KnownEntrypoint || knownEntrypoint
		evidence.CCH = evidence.CCH || hasCCH
		complete = complete || (hasVersion && knownEntrypoint && hasCCH)
	}
	return evidence, complete
}

func systemTexts(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		texts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text, ok := block["text"].(string)
			if ok {
				texts = append(texts, text)
			}
		}
		return texts
	default:
		return nil
	}
}

func parseBillingFields(value string) map[string]string {
	fields := make(map[string]string)
	for _, raw := range strings.Split(value, ";") {
		key, fieldValue, found := strings.Cut(raw, "=")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			fields[key] = strings.TrimSpace(fieldValue)
		}
	}
	return fields
}

func hasStructuredMetadata(value any) bool {
	metadata, ok := value.(map[string]any)
	if !ok {
		return false
	}
	userID, ok := metadata["user_id"].(string)
	if !ok || strings.TrimSpace(userID) == "" {
		return false
	}
	var identity struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(userID), &identity); err != nil {
		return false
	}
	return strings.TrimSpace(identity.DeviceID) != "" &&
		strings.TrimSpace(identity.AccountUUID) != "" &&
		strings.TrimSpace(identity.SessionID) != ""
}
