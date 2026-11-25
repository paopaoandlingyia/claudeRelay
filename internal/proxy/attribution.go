package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/local/claude-relay/internal/credential"
)

const (
	billingAttributionPrefix = "x-anthropic-billing-header:"
	// Opus 5.5 requires Claude Code 2.1.280 or newer. Use this baseline for
	// account checks and requests without an aligned, recognized CLI release.
	defaultClaudeCodeVersion = "2.1.280"
	// This is a compatibility identifier, not a claim that claude-relay is Claude Desktop.
	defaultBillingAttribution = "x-anthropic-billing-header: cc_version=" + defaultClaudeCodeVersion + "; cc_entrypoint=claude-desktop-3p;"
	// Used when the outgoing request has no versioned Claude Code user agent.
	defaultClientUserAgent = "claude-cli/" + defaultClaudeCodeVersion + " (external, claude-desktop)"
)

var sessionHeaderNames = []string{
	claudeCodeSessionHeader,
	"X-Claude-Session-Id",
	"X-Session-Id",
	"Session-Id",
}

const claudeCodeSessionHeader = "X-Claude-Code-Session-Id"

type attributionUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

type attributionOptions struct {
	IncludeMetadata      bool
	IncludeSessionHeader bool
	SessionSeed          string
	AlignGeneratedFields bool
}

type subscriptionAttribution struct {
	Body    []byte
	Changed bool
	// SessionHeader applies only to the outgoing request. Routing uses the original.
	SessionHeader string
}

// addSubscriptionAttribution adds only the minimum fields demonstrated by the
// protocol experiments. Existing caller-owned billing and metadata fields are
// preserved, including fields the relay does not interpret.
func addSubscriptionAttribution(body *requestBody, headers http.Header, cred credential.Credential, options attributionOptions) (subscriptionAttribution, error) {
	result := subscriptionAttribution{}
	attempt := *body
	attempt.root = cloneMap(body.root)
	root := attempt.root

	system, billingFound, err := inspectSystem(root["system"])
	if err != nil {
		return subscriptionAttribution{}, err
	}

	changed := false
	if !billingFound {
		billingText := defaultBillingAttribution
		if options.AlignGeneratedFields {
			userAgent := headers.Get("User-Agent")
			tokens := strings.Fields(userAgent)
			if version := extractClaudeCodeUAVersion(userAgent); version != "" && len(tokens) > 0 && strings.EqualFold(tokens[0], "claude-cli/"+version) {
				// Match the existing outgoing CLI release without inventing a CLI
				// User-Agent, fingerprint suffix, or CCH. Keep the tested API entrypoint.
				billingText = billingAttributionPrefix + " cc_version=" + version + "; cc_entrypoint=claude-desktop-3p;"
			}
		}
		billing := map[string]any{"type": "text", "text": billingText}
		system = append([]any{billing}, system...)
		root["system"] = system
		changed = true
	}

	if options.IncludeMetadata {
		metadata, err := metadataObject(root["metadata"])
		if err != nil {
			return subscriptionAttribution{}, err
		}
		if existing, _ := metadata["user_id"].(string); strings.TrimSpace(existing) == "" {
			sessionID, err := attributionSessionID(headers, cred.DeviceID, options.SessionSeed)
			if err != nil {
				return subscriptionAttribution{}, err
			}
			value, err := json.Marshal(attributionUserID{
				DeviceID:    cred.DeviceID,
				AccountUUID: cred.AccountUUID,
				SessionID:   sessionID,
			})
			if err != nil {
				return subscriptionAttribution{}, fmt.Errorf("encode metadata user identity: %w", err)
			}
			// The next account attempt must see the caller's original metadata.
			metadata = cloneMap(metadata)
			metadata["user_id"] = string(value)
			root["metadata"] = metadata
			changed = true
			if options.AlignGeneratedFields && strings.TrimSpace(headers.Get(claudeCodeSessionHeader)) != "" {
				result.SessionHeader = sessionID
			}
		}
	}
	if options.IncludeSessionHeader {
		result.SessionHeader = validRequestID(originalMetadataSession(root["metadata"]))
		if result.SessionHeader == "" {
			result.SessionHeader, err = attributionSessionID(headers, cred.DeviceID, options.SessionSeed)
			if err != nil {
				return subscriptionAttribution{}, err
			}
		}
	}

	attempt.changed = attempt.changed || changed
	encoded, err := attempt.bytes()
	if err != nil {
		return subscriptionAttribution{}, fmt.Errorf("encode attributed request body: %w", err)
	}
	result.Body, result.Changed = encoded, changed
	return result, nil
}

func inspectSystem(value any) (system []any, billingFound bool, err error) {
	switch typed := value.(type) {
	case nil:
		return nil, false, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, false, nil
		}
		billingFound = strings.HasPrefix(typed, billingAttributionPrefix)
		return []any{map[string]any{"type": "text", "text": typed}}, billingFound, nil
	case []any:
		system = typed
	default:
		return nil, false, fmt.Errorf("system must be a string or an array")
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
	}
	return system, billingFound, nil
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

func attributionSessionID(headers http.Header, deviceID, fallbackSeed string) (string, error) {
	for _, name := range sessionHeaderNames {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return deterministicUUID(deviceID, value), nil
		}
	}
	if fallbackSeed != "" {
		return deterministicUUID(deviceID, fallbackSeed), nil
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
