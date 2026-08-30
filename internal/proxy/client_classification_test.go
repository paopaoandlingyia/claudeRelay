package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/local/claude-relay/internal/store"
)

const testClaudeCodeMetadata = `{"device_id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","account_uuid":"","session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`

func TestClassifyClientRequestKinds(t *testing.T) {
	t.Parallel()
	validHeaders := func() http.Header {
		headers := http.Header{}
		headers.Set("User-Agent", "claude-cli/2.1.247 (external, sdk-cli)")
		headers.Set(claudeCodeSessionHeader, "session")
		headers.Set("X-App", "cli")
		headers.Set("anthropic-beta", "claude-code-20250219")
		headers.Set("anthropic-version", "2023-06-01")
		return headers
	}
	metadata := `"metadata":{"user_id":` + strconv.Quote(testClaudeCodeMetadata) + `}`
	tests := []struct {
		name      string
		path      string
		body      string
		headers   http.Header
		wantClass string
		wantKind  string
	}{
		{
			name:    "ordinary messages with billing attribution",
			path:    "/v1/messages",
			body:    `{"model":"claude-sonnet-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.247.abc; cc_entrypoint=sdk-cli;"}],` + metadata + `,"messages":[]}`,
			headers: validHeaders(), wantClass: clientClassCCCandidate, wantKind: clientKindMessages,
		},
		{
			name:    "ordinary messages with official prompt",
			path:    "/v1/messages",
			body:    `{"model":"claude-sonnet-5","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],` + metadata + `,"messages":[]}`,
			headers: validHeaders(), wantClass: clientClassCCCandidate, wantKind: clientKindMessages,
		},
		{
			name:    "count tokens needs no body attribution",
			path:    "/v1/messages/count_tokens",
			body:    `{"model":"claude-sonnet-5","messages":[],"tools":[]}`,
			headers: validHeaders(), wantClass: clientClassCCCandidate, wantKind: clientKindCountTokens,
		},
		{
			name:    "messages token-count fallback",
			path:    "/v1/messages",
			body:    `{"model":"claude-sonnet-5","max_tokens":1,"messages":[],` + metadata + `}`,
			headers: validHeaders(), wantClass: clientClassCCCandidate, wantKind: clientKindTokenCountFallback,
		},
		{
			name:    "haiku connectivity probe",
			path:    "/v1/messages",
			body:    `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[]}`,
			headers: validHeaders(), wantClass: clientClassCCCandidate, wantKind: clientKindHaikuProbe,
		},
		{
			name:    "headers alone do not admit ordinary messages",
			path:    "/v1/messages",
			body:    `{"model":"claude-sonnet-5","messages":[]}`,
			headers: validHeaders(), wantClass: clientClassAmbiguous, wantKind: clientKindAmbiguous,
		},
		{
			name:      "missing protocol header rejects fallback",
			path:      "/v1/messages",
			body:      `{"model":"claude-sonnet-5","max_tokens":1,"messages":[],` + metadata + `}`,
			headers:   func() http.Header { h := validHeaders(); h.Del("anthropic-beta"); return h }(),
			wantClass: clientClassAmbiguous, wantKind: clientKindAmbiguous,
		},
		{
			name: "user agent must start with a versioned claude cli token",
			path: "/v1/messages/count_tokens",
			body: `{"model":"claude-sonnet-5","messages":[]}`,
			headers: func() http.Header {
				h := validHeaders()
				h.Set("User-Agent", "third-party claude-cli/2.1.247")
				return h
			}(),
			wantClass: clientClassAmbiguous, wantKind: clientKindAmbiguous,
		},
		{
			name:    "ordinary compatible request",
			path:    "/v1/messages",
			body:    `{"model":"claude-sonnet-5","messages":[]}`,
			headers: http.Header{}, wantClass: clientClassCompatible, wantKind: clientKindCompatible,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			route, err := deriveRequestRoute([]byte(test.body), test.headers, store.AccountPoolCompatible, test.path)
			if err != nil {
				t.Fatal(err)
			}
			if route.Client.Class != test.wantClass || route.Client.Kind != test.wantKind {
				t.Fatalf("classification = %q/%q, want %q/%q", route.Client.Class, route.Client.Kind, test.wantClass, test.wantKind)
			}
		})
	}
}

func TestStructuredMetadataAcceptsCurrentAndLegacyClaudeCodeFormats(t *testing.T) {
	t.Parallel()
	legacy := "user_" + strings.Repeat("a", 64) + "_account__session_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, userID := range []string{testClaudeCodeMetadata, legacy} {
		if !hasStructuredMetadata(map[string]any{"user_id": userID}) {
			t.Fatalf("metadata user_id was rejected: %q", userID)
		}
	}
	for _, userID := range []string{
		`{"device_id":"device","account_uuid":"","session_id":""}`,
		`{"device_id":"","account_uuid":"","session_id":"session"}`,
		"not-claude-code-metadata",
	} {
		if hasStructuredMetadata(map[string]any{"user_id": userID}) {
			t.Fatalf("invalid metadata user_id was accepted: %q", userID)
		}
	}
}
