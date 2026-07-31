package proxy

import (
	"net/http"
	"testing"

	"github.com/local/claude-relay/internal/store"
)

func TestClassifyClientEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		body           string
		userAgent      string
		session        string
		xApp           string
		wantClass      string
		wantCCH        bool
		wantMeta       bool
		wantUA         bool
		wantEntrypoint bool
		wantSession    bool
		wantXApp       bool
	}{
		{
			name:           "complete official-shaped request",
			body:           `{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=abcde;"}],"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"session\"}"}}`,
			wantClass:      clientClassCCCandidate,
			wantCCH:        true,
			wantMeta:       true,
			wantEntrypoint: true,
		},
		{
			name:           "third party Claude Code API mode",
			body:           `{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop-3p;"}],"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"\",\"session_id\":\"session\"}"}}`,
			userAgent:      "claude-cli/2.1.219 (external, claude-desktop-3p, agent-sdk/0.3.219)",
			session:        "session",
			xApp:           "cli",
			wantClass:      clientClassCCCandidate,
			wantUA:         true,
			wantEntrypoint: true,
			wantSession:    true,
			wantXApp:       true,
		},
		{
			name:           "billing without metadata",
			body:           `{"model":"claude-test","system":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=abcde;"}`,
			wantClass:      clientClassAmbiguous,
			wantCCH:        true,
			wantEntrypoint: true,
		},
		{
			name:           "empty cch",
			body:           `{"model":"claude-test","system":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=;","metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"session\"}"}}`,
			wantClass:      clientClassAmbiguous,
			wantMeta:       true,
			wantEntrypoint: true,
		},
		{
			name:      "user agent only",
			body:      `{"model":"claude-test"}`,
			userAgent: "claude-cli/2.1.219",
			wantClass: clientClassAmbiguous,
			wantUA:    true,
		},
		{
			name:        "Claude headers missing x-app",
			body:        `{"model":"claude-test"}`,
			userAgent:   "claude-cli/2.1.219",
			session:     "session",
			wantClass:   clientClassAmbiguous,
			wantUA:      true,
			wantSession: true,
		},
		{
			name:      "ordinary request",
			body:      `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`,
			wantClass: clientClassCompatible,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			if test.userAgent != "" {
				headers.Set("User-Agent", test.userAgent)
			}
			if test.session != "" {
				headers.Set(claudeCodeSessionHeader, test.session)
			}
			if test.xApp != "" {
				headers.Set("X-App", test.xApp)
			}
			route, err := deriveRequestRoute([]byte(test.body), headers, store.AccountPoolCompatible)
			if err != nil {
				t.Fatal(err)
			}
			if route.Client.Class != test.wantClass {
				t.Errorf("class = %q, want %q", route.Client.Class, test.wantClass)
			}
			if route.Client.Evidence.CCH != test.wantCCH {
				t.Errorf("cch evidence = %v, want %v", route.Client.Evidence.CCH, test.wantCCH)
			}
			if route.Client.Evidence.StructuredMetadata != test.wantMeta {
				t.Errorf("metadata evidence = %v, want %v", route.Client.Evidence.StructuredMetadata, test.wantMeta)
			}
			if route.Client.Evidence.ClaudeUserAgent != test.wantUA {
				t.Errorf("user agent evidence = %v, want %v", route.Client.Evidence.ClaudeUserAgent, test.wantUA)
			}
			if route.Client.Evidence.KnownEntrypoint != test.wantEntrypoint {
				t.Errorf("entrypoint evidence = %v, want %v", route.Client.Evidence.KnownEntrypoint, test.wantEntrypoint)
			}
			if route.Client.Evidence.ClaudeCodeSession != test.wantSession {
				t.Errorf("Claude Code session evidence = %v, want %v", route.Client.Evidence.ClaudeCodeSession, test.wantSession)
			}
			if route.Client.Evidence.XAppCLI != test.wantXApp {
				t.Errorf("x-app evidence = %v, want %v", route.Client.Evidence.XAppCLI, test.wantXApp)
			}
		})
	}
}
