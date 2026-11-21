package proxy

import (
	"net/http"
	"strings"
)

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyRequestHeaders(destination, source http.Header, policy ingressPolicy) {
	if policy.Name == ingressCompatible {
		copyCompatibleRequestHeaders(destination, source)
		return
	}

	copyHeaders(destination, source)
	destination.Del("Host")
	destination.Del("Content-Length")
	destination.Del(accountHeader)
	destination.Del(requestIDHeader)
	// Adapt browser clients only on compatible ingresses. Official traffic keeps
	// these signals so upstream browser-access checks still apply.
	if policy.StripBrowserHeaders {
		destination.Del("Origin")
		destination.Del("anthropic-dangerous-direct-browser-access")
	}
}

func copyCompatibleRequestHeaders(destination, source http.Header) {
	for _, name := range []string{
		"Accept",
		"Content-Type",
		"Anthropic-Version",
		"Anthropic-Beta",
		"Anthropic-Workspace-Id",
		"Anthropic-User-Profile-Id",
	} {
		for _, value := range source.Values(name) {
			if strings.TrimSpace(value) != "" {
				destination.Add(name, value)
			}
		}
	}

	var evidence clientEvidence
	setHeaderEvidence(&evidence, source)
	userAgent := defaultClientUserAgent
	if evidence.ClaudeUserAgent {
		userAgent = source.Get("User-Agent")
	}
	destination.Set("User-Agent", userAgent)
	app := "cli"
	if evidence.ClaudeUserAgent && evidence.XAppCLI {
		app = source.Get("X-App")
	}
	destination.Set("X-App", app)
	if evidence.ClaudeCodeSession {
		destination.Set(claudeCodeSessionHeader, source.Get(claudeCodeSessionHeader))
	}
	if destination.Get("Anthropic-Beta") == "" {
		destination.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
}

func copyResponseHeaders(destination, source http.Header) {
	copyHeaders(destination, source)
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		if _, excluded := hopByHopHeaders[http.CanonicalHeaderKey(key)]; excluded {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
