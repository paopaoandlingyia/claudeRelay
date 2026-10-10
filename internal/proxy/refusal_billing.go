package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"sync"

	"github.com/local/claude-relay/internal/metrics"
	"github.com/local/claude-relay/internal/store"
)

const refusalBillingSetting = "refusal_billing"

// Bound only adapted payloads; large streaming replies remain event-by-event.
const maxAdaptedResponseBytes = 32 << 20

type refusalBillingOptions struct {
	Enabled     bool               `json:"enabled"`
	Multipliers map[string]float64 `json:"multipliers"`
}

type refusalBillingPolicy struct {
	mu      sync.RWMutex
	options refusalBillingOptions
}

func loadRefusalBillingPolicy(database *store.Store) (*refusalBillingPolicy, error) {
	options := refusalBillingOptions{Multipliers: map[string]float64{
		"cyber": 2, "reasoning_extraction": 3, "bio": 5, "frontier_llm": 5, "general_harms": 5,
	}}
	value, found, err := database.RuntimeSetting(context.Background(), refusalBillingSetting)
	if err != nil {
		return nil, err
	}
	if found {
		options = refusalBillingOptions{}
		if err := json.Unmarshal([]byte(value), &options); err != nil {
			return nil, fmt.Errorf("decode persisted refusal billing: %w", err)
		}
		if err := validateRefusalBillingOptions(options); err != nil {
			return nil, fmt.Errorf("validate persisted refusal billing: %w", err)
		}
	}
	return &refusalBillingPolicy{options: options}, nil
}

func validateRefusalBillingOptions(options refusalBillingOptions) error {
	if len(options.Multipliers) != 5 {
		return fmt.Errorf("multipliers must contain exactly the five refusal categories")
	}
	for _, category := range []string{"cyber", "reasoning_extraction", "bio", "frontier_llm", "general_harms"} {
		factor, exists := options.Multipliers[category]
		if !exists || math.IsNaN(factor) || math.IsInf(factor, 0) || factor < 1 || factor > 100 {
			return fmt.Errorf("%s multiplier must be a finite number between 1 and 100", category)
		}
	}
	return nil
}

// Each request owns a snapshot, including its map, so saving new settings cannot
// change an in-flight response or race with its token adjustment.
func (p *refusalBillingPolicy) current() refusalBillingOptions {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return refusalBillingOptions{Enabled: p.options.Enabled, Multipliers: maps.Clone(p.options.Multipliers)}
}

func (p *refusalBillingPolicy) persist(ctx context.Context, database *store.Store, options refusalBillingOptions) error {
	if err := validateRefusalBillingOptions(options); err != nil {
		return err
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("encode refusal billing: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := database.SetRuntimeSetting(ctx, refusalBillingSetting, string(encoded)); err != nil {
		return err
	}
	p.options = refusalBillingOptions{Enabled: options.Enabled, Multipliers: maps.Clone(options.Multipliers)}
	return nil
}

func (s *Server) setRefusalBilling(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled     *bool              `json:"enabled"`
		Multipliers map[string]float64 `json:"multipliers"`
	}
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "enabled is required")
		return
	}
	options := refusalBillingOptions{Enabled: *request.Enabled, Multipliers: request.Multipliers}
	if err := validateRefusalBillingOptions(options); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.refusalBilling.persist(r.Context(), s.store, options); err != nil {
		slog.Error("persist refusal billing", "request_id", requestIDFromContext(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist refusal billing")
		return
	}
	slog.Info("refusal billing changed", "request_id", requestIDFromContext(r.Context()),
		"enabled", options.Enabled, "multipliers", options.Multipliers, "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, options)
}

// The accounting.Observer reads original upstream bytes before this adapter.
// Only downstream billing usage changes; account consumption stays authentic.
type refusalResponseAdapter struct {
	options      refusalBillingOptions
	requestID    string
	nonStreaming bool
	usage        map[string]json.RawMessage
	billing      *metrics.RefusalBilling
}

func (a *refusalResponseAdapter) adjust(body []byte) ([]byte, bool, error) {
	if !bytes.Contains(body, []byte(`"message_start"`)) &&
		!bytes.Contains(body, []byte(`"message_delta"`)) && !bytes.Contains(body, []byte(`"refusal"`)) {
		return body, false, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, fmt.Errorf("decode refusal billing response: %w", err)
	}
	var kind string
	if raw := root["type"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &kind); err != nil {
			return nil, false, fmt.Errorf("decode response type: %w", err)
		}
	} else if a.nonStreaming && len(root["stop_reason"]) > 0 && len(root["usage"]) > 0 {
		// Some upstream JSON replies omit type although they contain the normal
		// Messages stop reason and usage. Match the accounting observer's support
		// for those replies; SSE events still require their explicit event type.
		kind = "message"
		slog.Warn("upstream Messages response missing type", "request_id", a.requestID)
	}
	envelope := root
	if kind == "message_start" {
		if err := json.Unmarshal(root["message"], &envelope); err != nil {
			return nil, false, fmt.Errorf("decode message_start: %w", err)
		}
	}
	if kind == "message" || kind == "message_start" || kind == "message_delta" {
		if raw := envelope["usage"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			var patch map[string]json.RawMessage
			if err := json.Unmarshal(raw, &patch); err != nil {
				return nil, false, fmt.Errorf("decode response usage: %w", err)
			}
			for key, value := range patch {
				if key == "cache_creation" && !bytes.Equal(value, []byte("null")) {
					merged := map[string]json.RawMessage{}
					if old := a.usage[key]; len(old) > 0 && !bytes.Equal(old, []byte("null")) {
						if err := json.Unmarshal(old, &merged); err != nil {
							return nil, false, fmt.Errorf("decode previous cache creation: %w", err)
						}
					}
					var detail map[string]json.RawMessage
					if err := json.Unmarshal(value, &detail); err != nil {
						return nil, false, fmt.Errorf("decode cache creation: %w", err)
					}
					maps.Copy(merged, detail)
					var err error
					value, err = json.Marshal(merged)
					if err != nil {
						return nil, false, fmt.Errorf("encode cache creation: %w", err)
					}
				}
				a.usage[key] = value
			}
		}
	}
	if kind != "message" && kind != "message_delta" {
		return body, false, nil
	}
	stop := root
	if kind == "message_delta" {
		if raw := root["delta"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if err := json.Unmarshal(raw, &stop); err != nil {
				return nil, false, fmt.Errorf("decode message_delta: %w", err)
			}
		} else {
			return body, false, nil
		}
	}
	var reason string
	if raw := stop["stop_reason"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &reason); err != nil {
			return nil, false, fmt.Errorf("decode stop reason: %w", err)
		}
	}
	if reason != "refusal" {
		return body, false, nil
	}
	var details struct {
		Category *string `json:"category"`
	}
	if raw := stop["stop_details"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &details); err != nil {
			return nil, false, fmt.Errorf("decode stop details: %w", err)
		}
	}
	if details.Category == nil {
		return body, false, nil
	}
	factor := a.options.Multipliers[*details.Category]
	if factor <= 1 {
		return body, false, nil
	}
	if len(a.usage) == 0 {
		return nil, false, fmt.Errorf("refusal billing requires upstream token usage")
	}
	billed := maps.Clone(a.usage)
	audit := &metrics.RefusalBilling{Category: *details.Category, Multiplier: factor,
		OriginalUsage: map[string]int64{}, BilledUsage: map[string]int64{}}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if raw, exists := billed[key]; exists {
			value, original, adjusted, err := scaleRefusalTokens(raw, factor)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", key, err)
			}
			billed[key] = value
			audit.OriginalUsage[key], audit.BilledUsage[key] = original, adjusted
		}
	}
	if raw := billed["cache_creation"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var detail map[string]json.RawMessage
		if err := json.Unmarshal(raw, &detail); err != nil {
			return nil, false, fmt.Errorf("decode billed cache creation: %w", err)
		}
		for _, key := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
			if raw, exists := detail[key]; exists {
				value, original, adjusted, err := scaleRefusalTokens(raw, factor)
				if err != nil {
					return nil, false, fmt.Errorf("%s: %w", key, err)
				}
				detail[key] = value
				audit.OriginalUsage[key], audit.BilledUsage[key] = original, adjusted
			}
		}
		var err error
		billed["cache_creation"], err = json.Marshal(detail)
		if err != nil {
			return nil, false, fmt.Errorf("encode billed cache creation: %w", err)
		}
	}
	var err error
	root["usage"], err = json.Marshal(billed)
	if err != nil {
		return nil, false, fmt.Errorf("encode billed usage: %w", err)
	}
	result, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode refusal billing response: %w", err)
	}
	a.billing = audit
	slog.Warn("refusal billing usage adjusted", "request_id", a.requestID, "category", audit.Category,
		"multiplier", factor, "original_usage", audit.OriginalUsage, "billed_usage", audit.BilledUsage)
	return result, true, nil
}

// Upstream counts are untrusted. Fail instead of wrapping or clamping: a
// malformed usage field cannot safely become an invented downstream charge.
func scaleRefusalTokens(raw json.RawMessage, factor float64) (json.RawMessage, int64, int64, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, 0, 0, fmt.Errorf("token count must be an integer")
	}
	var original int64
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, 0, 0, fmt.Errorf("decode token count: %w", err)
	}
	adjusted := math.Ceil(float64(original) * factor)
	if original < 0 || math.IsNaN(adjusted) || math.IsInf(adjusted, 0) || adjusted < 0 || adjusted > math.MaxInt32 {
		return nil, 0, 0, fmt.Errorf("token count %d cannot safely be multiplied by %g", original, factor)
	}
	count := int64(adjusted)
	encoded, err := json.Marshal(count)
	return encoded, original, count, err
}
