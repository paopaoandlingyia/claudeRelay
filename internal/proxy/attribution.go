package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/local/claude-relay/internal/credential"
)

const (
	billingAttributionPrefix = "x-anthropic-billing-header:"
	// Captured from the official Claude Desktop / Claude Code 2.1.219 build on 2026-07-30.
	// This is a compatibility identifier, not a claim that claude-relay is Claude Desktop.
	observedBillingAttribution = "x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop;"
)

var sessionHeaderNames = []string{
	"X-Claude-Session-Id",
	"X-Session-Id",
	"Session-Id",
}

type attributionUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// addSubscriptionAttribution adds only the minimum fields demonstrated by the
// protocol experiments. A request carrying an existing CCH is returned byte-for-byte
// because changing any body byte could invalidate an official client signature.
func addSubscriptionAttribution(body []byte, headers http.Header, cred credential.Credential, includeMetadata bool) ([]byte, bool, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, false, fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false, fmt.Errorf("decode request body: trailing JSON value")
	}
	if root == nil {
		return nil, false, fmt.Errorf("request body must be a JSON object")
	}

	system, billingFound, signedBilling, err := inspectSystem(root["system"])
	if err != nil {
		return nil, false, err
	}
	if signedBilling {
		return body, false, nil
	}

	changed := false
	if !billingFound {
		billing := map[string]any{"type": "text", "text": observedBillingAttribution}
		system = append([]any{billing}, system...)
		root["system"] = system
		changed = true
	}

	if !includeMetadata {
		if !changed {
			return body, false, nil
		}
		transformed, err := json.Marshal(root)
		if err != nil {
			return nil, false, fmt.Errorf("encode attributed request body: %w", err)
		}
		return transformed, true, nil
	}

	metadata, err := metadataObject(root["metadata"])
	if err != nil {
		return nil, false, err
	}
	if existing, _ := metadata["user_id"].(string); strings.TrimSpace(existing) == "" {
		sessionID, err := attributionSessionID(headers, cred.DeviceID)
		if err != nil {
			return nil, false, err
		}
		value, err := json.Marshal(attributionUserID{
			DeviceID:    cred.DeviceID,
			AccountUUID: cred.AccountUUID,
			SessionID:   sessionID,
		})
		if err != nil {
			return nil, false, fmt.Errorf("encode metadata user identity: %w", err)
		}
		metadata["user_id"] = string(value)
		root["metadata"] = metadata
		changed = true
	}

	if !changed {
		return body, false, nil
	}
	transformed, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode attributed request body: %w", err)
	}
	return transformed, true, nil
}

func inspectSystem(value any) (system []any, billingFound, signedBilling bool, err error) {
	switch typed := value.(type) {
	case nil:
		return nil, false, false, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, false, false, nil
		}
		billingFound = strings.HasPrefix(typed, billingAttributionPrefix)
		signedBilling = billingFound && strings.Contains(typed, "cch=")
		return []any{map[string]any{"type": "text", "text": typed}}, billingFound, signedBilling, nil
	case []any:
		system = typed
	default:
		return nil, false, false, fmt.Errorf("system must be a string or an array")
	}

	for _, item := range system {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text, _ := block["text"].(string)
		if !strings.HasPrefix(text, billingAttributionPrefix) {
			continue
		}
		billingFound = true
		if strings.Contains(text, "cch=") {
			signedBilling = true
		}
	}
	return system, billingFound, signedBilling, nil
}

func metadataObject(value any) (map[string]any, error) {
	if value == nil {
		return make(map[string]any), nil
	}
	metadata, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("metadata must be an object")
	}
	if existing, exists := metadata["user_id"]; exists {
		if _, ok := existing.(string); !ok {
			return nil, fmt.Errorf("metadata.user_id must be a string")
		}
	}
	return metadata, nil
}

func attributionSessionID(headers http.Header, deviceID string) (string, error) {
	for _, name := range sessionHeaderNames {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return deterministicUUID(deviceID, value), nil
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate request session identity: %w", err)
	}
	setUUIDVersion(raw, 4)
	return formatUUID(raw), nil
}

func deterministicUUID(deviceID, session string) string {
	sum := sha256.Sum256([]byte(deviceID + "\x00" + session))
	raw := append([]byte(nil), sum[:16]...)
	setUUIDVersion(raw, 5)
	return formatUUID(raw)
}

func setUUIDVersion(raw []byte, version byte) {
	raw[6] = (raw[6] & 0x0f) | (version << 4)
	raw[8] = (raw[8] & 0x3f) | 0x80
}

func formatUUID(raw []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
