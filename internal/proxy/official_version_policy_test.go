package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfficialVersionPolicyComparesSemanticVersions(t *testing.T) {
	policy := &officialVersionPolicy{}
	policy.set(officialVersionBounds{Min: "2.1.280", Max: "2.1.300"})

	for _, test := range []struct {
		name string
		ua   string
		want string
	}{
		{name: "minimum accepted", ua: "claude-cli/2.1.280 (cli)", want: ""},
		{name: "maximum accepted", ua: "claude-cli/2.1.300 (cli)", want: ""},
		{name: "below minimum", ua: "claude-cli/2.1.279 (cli)", want: "below"},
		{name: "above maximum", ua: "claude-cli/2.1.301 (cli)", want: "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := policy.check(test.ua)
			if test.want == "" && err != nil {
				t.Fatalf("check(%q) = %v, want success", test.ua, err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("check(%q) = %v, want error containing %q", test.ua, err, test.want)
			}
		})
	}
}

func TestOfficialVersionBoundsApplyOnlyOfficialIngressAndPersist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1<<20)

	set := adminRequest(t, server, http.MethodPost, "/admin/v1/official/version-bounds", `{"min_version":"2.1.300","max_version":"2.1.400"}`)
	if set.Code != http.StatusOK {
		t.Fatalf("set bounds status = %d, body = %s", set.Code, set.Body.String())
	}
	overview := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
	if !strings.Contains(overview.Body.String(), `"official_min_cli_version":"2.1.300"`) {
		t.Fatalf("overview did not expose minimum version: %s", overview.Body.String())
	}

	official := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(officialClaudeCodeTestBody()))
	official.Header.Set("x-api-key", "official-downstream-key")
	setOfficialClaudeCodeTestHeaders(official)
	rejected := httptest.NewRecorder()
	server.routes().ServeHTTP(rejected, official)
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "below the configured minimum") {
		t.Fatalf("official request status = %d, body = %s", rejected.Code, rejected.Body.String())
	}

	compatible := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[]}`))
	compatible.Header.Set("x-api-key", "downstream-key")
	accepted := httptest.NewRecorder()
	server.routes().ServeHTTP(accepted, compatible)
	if accepted.Code != http.StatusOK {
		t.Fatalf("compatible request status = %d, body = %s", accepted.Code, accepted.Body.String())
	}

	persisted, err := NewServer(server.cfg, server.store)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.officialVersion.current(); got.Min != "2.1.300" || got.Max != "2.1.400" {
		t.Fatalf("persisted bounds = %#v", got)
	}
}

func TestSetOfficialVersionBoundsRejectsInvalidRanges(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1024)
	for _, body := range []string{
		`{"min_version":"2.1","max_version":""}`,
		`{"min_version":"2.1.400","max_version":"2.1.300"}`,
	} {
		recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/official/version-bounds", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, recorder.Code)
		}
	}
}
