package proxy

import (
	"net/http"
	"testing"
)

func TestClassifyClientEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		userAgent string
		wantClass string
		wantCCH   bool
		wantMeta  bool
		wantUA    bool
	}{
		{
			name:      "complete official-shaped request",
			body:      `{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=abcde;"}],"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"session\"}"}}`,
			wantClass: clientClassCCCandidate,
			wantCCH:   true,
			wantMeta:  true,
		},
		{
			name:      "billing without metadata",
			body:      `{"model":"claude-test","system":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=abcde;"}`,
			wantClass: clientClassAmbiguous,
			wantCCH:   true,
		},
		{
			name:      "empty cch",
			body:      `{"model":"claude-test","system":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=;","metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"session\"}"}}`,
			wantClass: clientClassAmbiguous,
			wantMeta:  true,
		},
		{
			name:      "user agent only",
			body:      `{"model":"claude-test"}`,
			userAgent: "claude-cli/2.1.219",
			wantClass: clientClassAmbiguous,
			wantUA:    true,
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
			route, err := deriveRequestRoute([]byte(test.body), headers, "relay-key")
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
		})
	}
}
