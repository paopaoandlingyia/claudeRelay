package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
)

func parseTestRequest(t testing.TB, raw []byte) *requestBody {
	t.Helper()
	body, err := parseRequestBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func normalizeTestToolNames(t testing.TB, raw []byte) ([]byte, int, toolNameMapping, error) {
	t.Helper()
	body := parseTestRequest(t, raw)
	changed, names, err := normalizeExperimentalToolNames(body)
	if err != nil {
		return nil, 0, nil, err
	}
	encoded, err := body.bytes()
	return encoded, changed, names, err
}

func TestParseRequestBody(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "[]", `"text"`, `{"messages":`, `{} {}`, `{} trailing`} {
		if _, err := parseRequestBody([]byte(raw)); err == nil {
			t.Errorf("accepted invalid request object %q", raw)
		}
	}
	raw := []byte("{\n  \"integer\": 9007199254740993, \"decimal\": 1.2300\n}\n")
	body := parseTestRequest(t, raw)
	if body.root["integer"] != json.Number("9007199254740993") || body.root["decimal"] != json.Number("1.2300") {
		t.Fatal("request numbers lost precision or representation")
	}
	encoded, err := body.bytes()
	if err != nil || !bytes.Equal(encoded, raw) {
		t.Fatalf("unchanged request bytes differ: %s (err=%v)", encoded, err)
	}
}

func TestRequestAttributionPreservesRetryBase(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"system":"keep these instructions","metadata":{"extra":{"integer":9007199254740993}},"tools":[{"name":"search"}],"messages":[{"role":"user","content":"hello"}],"future_field":{"decimal":1.2300}}`)
	body := parseTestRequest(t, raw)
	if _, _, err := normalizeExperimentalToolNames(body); err != nil {
		t.Fatal(err)
	}
	base, err := body.bytes()
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{claudeCodeSessionHeader: []string{"original-session"}}
	options := attributionOptions{IncludeMetadata: true, AlignGeneratedFields: true}
	var previous []byte
	for _, cred := range []credential.Credential{
		{DeviceID: "device-a", AccountUUID: "account-a"},
		{DeviceID: "device-b", AccountUUID: "account-b"},
	} {
		attributed, err := addSubscriptionAttribution(body, headers, cred, options)
		if err != nil {
			t.Fatal(err)
		}
		root := parseTestRequest(t, attributed.Body).root
		identity := decodeUserID(t, root)
		if identity.DeviceID != cred.DeviceID || identity.AccountUUID != cred.AccountUUID || identity.SessionID != deterministicUUID(cred.DeviceID, "original-session") || attributed.SessionHeader != identity.SessionID {
			t.Fatalf("incorrect per-account identity: %+v", identity)
		}
		if root["tools"].([]any)[0].(map[string]any)["name"] != "mcp__search" || !bytes.Contains(attributed.Body, []byte("9007199254740993")) || !bytes.Contains(attributed.Body, []byte("1.2300")) {
			t.Fatal("tool transformation or unknown numeric fields were lost")
		}
		if _, exists := body.root["metadata"].(map[string]any)["user_id"]; exists || body.root["system"] != "keep these instructions" {
			t.Fatal("attribution modified the retry base")
		}
		encoded, err := body.bytes()
		if err != nil || !bytes.Equal(encoded, base) || !bytes.Equal(body.raw, raw) {
			t.Fatal("attribution modified shared request content")
		}
		if previous != nil && bytes.Equal(previous, attributed.Body) {
			t.Fatal("failover reused the previous account's attribution")
		}
		previous = attributed.Body
	}
}

func TestForwardSharedRequestKeepsCompressionAndOriginalFingerprint(t *testing.T) {
	t.Parallel()
	const session = "original-session"
	const secondUUID = "22222222-2222-4222-8222-222222222222"
	type observed struct {
		identity attributionUserID
		body     []byte
	}
	seen := make(chan observed, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("large request did not retain upstream gzip")
			http.Error(w, "expected gzip", 400)
			return
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid gzip", 400)
			return
		}
		defer gz.Close()
		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid compressed body", 400)
			return
		}
		var root map[string]any
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		metadata, _ := root["metadata"].(map[string]any)
		userID, _ := metadata["user_id"].(string)
		var identity attributionUserID
		if err := json.Unmarshal([]byte(userID), &identity); err != nil {
			t.Error(err)
			http.Error(w, "invalid identity", 400)
			return
		}
		device, uuid := strings.Repeat("a", 64), "11111111-1111-4111-8111-111111111111"
		if r.Header.Get("Authorization") == "Bearer second-token" {
			device, uuid = strings.Repeat("b", 64), secondUUID
		}
		if identity.DeviceID != device || identity.AccountUUID != uuid || identity.SessionID != deterministicUUID(device, session) || r.Header.Get(claudeCodeSessionHeader) != identity.SessionID {
			t.Errorf("failover identity/header mismatch: %+v", identity)
		}
		seen <- observed{identity: identity, body: raw}
		if len(seen) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"stop_reason":"refusal","stop_details":{"category":"bio"},"usage":{"input_tokens":7,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1<<20)
	importTestAccount(t, server.store, "second", "second-token", secondUUID, "b")
	raw := []byte(`{"model":"claude-test","system":"keep me","metadata":{"extra":9007199254740993},"tools":[{"name":"search","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"search"},"messages":[{"role":"user","content":"` + strings.Repeat("synthetic context ", 1000) + `"}]}`)
	original := parseTestRequest(t, raw)
	keys, err := server.refusalGuard.fingerprints(original.root, nil, experimentalIngress.Name)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	request.Header.Set("x-api-key", "experimental-downstream-key")
	request.Header.Set(claudeCodeSessionHeader, session)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(seen) != 2 {
		t.Fatalf("status=%d upstream attempts=%d response=%s", recorder.Code, len(seen), recorder.Body.String())
	}
	first, second := <-seen, <-seen
	if first.identity.AccountUUID == second.identity.AccountUUID {
		t.Fatal("failover did not regenerate account attribution")
	}
	for _, attempt := range []observed{first, second} {
		root := parseTestRequest(t, attempt.body).root
		if root["tools"].([]any)[0].(map[string]any)["name"] != "mcp__search" || root["tool_choice"].(map[string]any)["name"] != "mcp__search" {
			t.Fatal("gzip request lost normalized tool names")
		}
		if !reflect.DeepEqual(root["messages"], original.root["messages"]) || root["metadata"].(map[string]any)["extra"] != json.Number("9007199254740993") {
			t.Fatal("gzip request changed caller content or numeric precision")
		}
	}
	state, err := server.store.RefusalCooldown(t.Context(), keys.lookup, time.Now())
	if err != nil || state.Strikes != 1 {
		t.Fatalf("refusal did not retain the original request fingerprint: %+v err=%v", state, err)
	}
	if _, _, err := normalizeExperimentalToolNames(original); err != nil {
		t.Fatal(err)
	}
	transformedKeys, err := server.refusalGuard.fingerprints(original.root, nil, experimentalIngress.Name)
	if err != nil {
		t.Fatal(err)
	}
	state, err = server.store.RefusalCooldown(t.Context(), transformedKeys.lookup, time.Now())
	if err != nil || state.Strikes != 0 {
		t.Fatalf("refusal used transformed tool names: %+v err=%v", state, err)
	}
	if request.Header.Get(claudeCodeSessionHeader) != session {
		t.Fatal("upstream attribution changed the original request header")
	}
}
