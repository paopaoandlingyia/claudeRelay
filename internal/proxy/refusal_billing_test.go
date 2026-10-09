package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/local/claude-relay/internal/accounting"
)

func TestRefusalBillingSettings(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 4096)
	options := server.refusalBilling.current()
	if options.Enabled || options.Multipliers["cyber"] != 2 || options.Multipliers["reasoning_extraction"] != 3 || options.Multipliers["bio"] != 5 {
		t.Fatalf("defaults = %+v", options)
	}
	options.Enabled = true
	options.Multipliers["cyber"] = 2.5
	body, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	saved := adminRequest(t, server, http.MethodPost, "/admin/v1/refusal-billing", string(body))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	loaded, err := NewServer(server.cfg, server.store)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.refusalBilling.current(); !got.Enabled || got.Multipliers["cyber"] != 2.5 {
		t.Fatalf("persisted settings=%+v", got)
	}
	options.Multipliers["cyber"] = 9
	if server.refusalBilling.current().Multipliers["cyber"] != 2.5 {
		t.Fatal("snapshot map mutated live policy")
	}
	for _, invalid := range []string{
		`{"multipliers":{"cyber":2,"reasoning_extraction":3,"bio":5,"frontier_llm":5,"general_harms":5}}`,
		`{"enabled":true,"multipliers":{"cyber":2}}`,
		`{"enabled":true,"multipliers":{"cyber":0,"reasoning_extraction":3,"bio":5,"frontier_llm":5,"general_harms":5}}`,
		`{"enabled":true,"multipliers":{"cyber":101,"reasoning_extraction":3,"bio":5,"frontier_llm":5,"general_harms":5}}`,
		`{"enabled":true,"multipliers":{"cyber":null,"reasoning_extraction":3,"bio":5,"frontier_llm":5,"general_harms":5}}`,
		`{"enabled":true,"multipliers":{"cyber":2,"reasoning_extraction":3,"bio":5,"frontier_llm":5,"general_harms":5,"future":5}}`,
	} {
		response := adminRequest(t, server, http.MethodPost, "/admin/v1/refusal-billing", invalid)
		if response.Code != http.StatusBadRequest || server.refusalBilling.current().Multipliers["cyber"] != 2.5 {
			t.Fatalf("invalid save changed settings: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	overview := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
	if overview.Code != 200 || !strings.Contains(overview.Body.String(), `"refusal_billing":{"enabled":true`) {
		t.Fatalf("missing overview settings: %s", overview.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/refusal-billing", bytes.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	unauthorized := httptest.NewRecorder()
	server.routes().ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("relay key can change billing: %d", unauthorized.Code)
	}
	if err := server.store.Close(); err != nil {
		t.Fatal(err)
	}
	options.Multipliers["cyber"] = 7
	if err := server.refusalBilling.persist(t.Context(), server.store, options); err == nil {
		t.Fatal("closed database accepted settings")
	}
	if server.refusalBilling.current().Multipliers["cyber"] != 2.5 {
		t.Fatal("failed persistence published new settings")
	}
}

func TestRefusalBillingForward(t *testing.T) {
	usage := `{"input_tokens":1000,"output_tokens":0,"cache_read_input_tokens":4000,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}`
	for _, tc := range []struct {
		name, category, reason string
		stream                 bool
		factor, output         int64
	}{
		{"json-bio", `"bio"`, "refusal", false, 5, 0},
		{"sse-bio", `"bio"`, "refusal", true, 5, 0},
		{"sse-midstream", `"bio"`, "refusal", true, 5, 20},
		{"sse-general-harms", `"general_harms"`, "refusal", true, 5, 0},
		{"sse-frontier", `"frontier_llm"`, "refusal", true, 5, 0},
		{"sse-cyber", `"cyber"`, "refusal", true, 2, 0},
		{"sse-reasoning", `"reasoning_extraction"`, "refusal", true, 3, 0},
		{"json-null", `null`, "refusal", false, 1, 0},
		{"sse-unknown", `"future"`, "refusal", true, 1, 0},
		{"sse-normal", `"bio"`, "end_turn", true, 1, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"type":"message","model":"claude-test","content":[],"stop_reason":"` + tc.reason + `","stop_details":{"category":` + tc.category + `},"usage":` + usage + `}`
			contentType := "application/json"
			if tc.stream {
				contentType = "text/event-stream"
				body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-test","model":"claude-test","usage":` + usage + `}}` + "\n\n" +
					"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"` + tc.reason + `","stop_details":{"category":` + tc.category + `}},"usage":{"output_tokens":` + strconv.FormatInt(tc.output, 10) + `}}` + "\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Encoding", "gzip")
				compressed := gzip.NewWriter(w)
				_, _ = io.WriteString(compressed, body)
				_ = compressed.Close()
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			options := server.refusalBilling.current()
			options.Enabled = true
			if err := server.refusalBilling.persist(t.Context(), server.store, options); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"harmless test"}]}`))
			request.Header.Set("x-api-key", "downstream-key")
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if recorder.Code != 200 || recorder.Header().Get("Content-Encoding") != "" {
				t.Fatalf("status=%d encoding=%s", recorder.Code, recorder.Header().Get("Content-Encoding"))
			}
			observer := accounting.NewObserver(bytes.NewReader(recorder.Body.Bytes()), contentType)
			if _, err := io.Copy(io.Discard, observer); err != nil {
				t.Fatal(err)
			}
			billed, _, _ := observer.Result(nil, "claude-test")
			if !billed.Complete || billed.InputTokens != 1000*tc.factor || billed.CacheReadTokens != 4000*tc.factor || billed.CacheCreation5mTokens != 100*tc.factor || billed.CacheCreation1hTokens != 200*tc.factor || billed.OutputTokens != tc.output*tc.factor {
				t.Fatalf("billed=%+v factor=%d", billed, tc.factor)
			}
			if tc.factor == 1 && recorder.Body.String() != body {
				t.Fatal("uncharged response changed")
			}
			if err := server.accounting.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			accounts, err := server.store.AllAccounts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			totals, err := server.store.UsageTotalsByModel(t.Context(), accounts[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			original := totals["claude-test"]
			if original.InputTokens != 1000 || original.CacheReadTokens != 4000 || original.OutputTokens != tc.output {
				t.Fatalf("original accounting changed: %+v", original)
			}
			records := server.metrics.Recent(1)
			if len(records) != 1 {
				t.Fatalf("records=%+v", records)
			}
			audit := records[0].RefusalBilling
			if tc.factor > 1 && (audit == nil || audit.Multiplier != float64(tc.factor) || audit.OriginalUsage["input_tokens"] != 1000 || audit.BilledUsage["input_tokens"] != billed.InputTokens) {
				t.Fatalf("missing billing audit: %+v", audit)
			}
			if tc.factor == 1 && audit != nil {
				t.Fatalf("unexpected adjustment: %+v", audit)
			}
			// Optional fixtures allow the other local gateways to consume the exact
			// production response without coupling this module to their dependencies.
			if dir := os.Getenv("REFUSAL_BILLING_FIXTURE_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.name+".response"), recorder.Body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRefusalBillingHotUpdateSnapshots(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var gate, releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","model":"m","stop_reason":"refusal","stop_details":{"category":"cyber"},"usage":{"input_tokens":100,"output_tokens":0}}`)
	}))
	defer upstream.Close()
	defer releaseOnce.Do(func() { close(release) })
	server := newTestServer(t, upstream.URL, 4096)
	options := server.refusalBilling.current()
	options.Enabled = true
	if err := server.refusalBilling.persist(t.Context(), server.store, options); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.routes().ServeHTTP(recorder, request); close(done) }()
	<-entered
	options.Multipliers["cyber"] = 7
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(t, server, http.MethodPost, "/admin/v1/refusal-billing", string(encoded))
	if response.Code != 200 {
		t.Fatalf("hot save=%d %s", response.Code, response.Body.String())
	}
	releaseOnce.Do(func() { close(release) })
	<-done
	if !strings.Contains(recorder.Body.String(), `"input_tokens":200`) {
		t.Fatalf("in-flight request changed: %s", recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder = httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), `"input_tokens":700`) {
		t.Fatalf("new request ignored hot save: %s", recorder.Body.String())
	}
}

func TestRefusalBillingSSECompositionAndFailures(t *testing.T) {
	options := refusalBillingOptions{Enabled: true, Multipliers: map[string]float64{"cyber": 2}}
	prefix := ": keepalive\r\nevent: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":30,\"cache_creation\":{\"ephemeral_5m_input_tokens\":10}}}}\r\n\r\n"
	tool := "event: content_block_start\r\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"name\":\"mcp__weather\",\"input\":{}}}\r\n\r\n"
	// A final cache detail is merged with the start; JSON spans two SSE data lines.
	suffix := "id: last\r\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"category\":\"cyber\"}},\r\ndata: \"usage\":{\"output_tokens\":0,\"cache_creation\":{\"ephemeral_1h_input_tokens\":20}}}\r\n\r\n"
	adapter := &refusalResponseAdapter{options: options, requestID: "test", usage: map[string]json.RawMessage{}}
	var output bytes.Buffer
	_, names, err := copySSEWithResponseTransforms(&output, strings.NewReader(prefix+tool+suffix), toolNameMapping{"mcp__weather": "weather"}, adapter)
	if err != nil || names != 1 {
		t.Fatalf("names=%d err=%v", names, err)
	}
	if !strings.HasPrefix(output.String(), prefix) || !strings.Contains(output.String(), `"name":"weather"`) || !strings.Contains(output.String(), "id: last\r\n") || !strings.Contains(output.String(), `"ephemeral_1h_input_tokens":40`) || !strings.Contains(output.String(), `"ephemeral_5m_input_tokens":20`) || !strings.Contains(output.String(), `"input_tokens":20`) {
		t.Fatalf("tool or billing transformation lost SSE data: %s", output.String())
	}
	adapter = &refusalResponseAdapter{options: options, usage: map[string]json.RawMessage{}}
	output.Reset()
	if _, _, err := copySSEWithResponseTransforms(&output, strings.NewReader(prefix+strings.TrimSuffix(suffix, "\r\n\r\n")), nil, adapter); !errors.Is(err, io.ErrUnexpectedEOF) || adapter.billing != nil {
		t.Fatalf("charged incomplete event: err=%v billing=%+v", err, adapter.billing)
	}
	for _, raw := range []string{`-1`, `2147483647`, `1.5`, `null`} {
		if _, _, _, err := scaleRefusalTokens(json.RawMessage(raw), 2); err == nil {
			t.Fatalf("accepted invalid token count %s", raw)
		}
	}
	encoded, original, billed, err := scaleRefusalTokens(json.RawMessage(`3`), 1.5)
	if err != nil || original != 3 || billed != 5 || string(encoded) != "5" {
		t.Fatalf("fractional factor: %s %d %d %v", encoded, original, billed, err)
	}
}

func TestRefusalBillingDisabledAndRequestIsolation(t *testing.T) {
	refused := `{"type":"message","model":"m","stop_reason":"refusal","stop_details":{"category":"cyber"},"usage":{"input_tokens":10,"output_tokens":0}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, refused)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	for _, tc := range []struct {
		enabled bool
		factor  float64
		path    string
		input   int
	}{
		{true, 2, "/v1/messages", 20},
		{false, 2, "/v1/messages", 10},
		{true, 1, "/v1/messages", 10},
		{true, 2, "/v1/messages/count_tokens", 10},
	} {
		options := server.refusalBilling.current()
		options.Enabled, options.Multipliers["cyber"] = tc.enabled, tc.factor
		if err := server.refusalBilling.persist(t.Context(), server.store, options); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"m","messages":[]}`))
		request.Header.Set("x-api-key", "downstream-key")
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"input_tokens":`+strconv.Itoa(tc.input)) {
			t.Fatalf("enabled=%v factor=%g path=%s response=%d %s", tc.enabled, tc.factor, tc.path, response.Code, response.Body.String())
		}
		if tc.input == 10 && (response.Body.String() != refused || server.metrics.Recent(1)[0].RefusalBilling != nil) {
			t.Fatal("unadjusted request inherited previous adjustment")
		}
	}
}

func TestRefusalBillingStreamsBeforeFinalRefusal(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	// A data line larger than bufio's read buffer must remain one SSE event.
	prefix := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", 5120) + "\"}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"category\":\"cyber\"}},\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}\n\n")
	}))
	defer upstream.Close()
	defer releaseOnce.Do(func() { close(release) })
	server := newTestServer(t, upstream.URL, 4096)
	options := server.refusalBilling.current()
	options.Enabled = true
	if err := server.refusalBilling.persist(t.Context(), server.store, options); err != nil {
		t.Fatal(err)
	}
	downstream := httptest.NewServer(server.routes())
	defer downstream.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, downstream.URL+"/v1/messages", strings.NewReader(`{"model":"m","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", "downstream-key")
	response, err := downstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var received strings.Builder
	for range 3 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		received.WriteString(line)
	}
	if received.String() != prefix {
		t.Fatal("stream prefix was buffered or changed")
	}
	releaseOnce.Do(func() { close(release) })
	final, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(final, []byte(`"input_tokens":20`)) {
		t.Fatalf("final response=%s error=%v", final, err)
	}
}
