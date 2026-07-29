package proxy

import "net/http"

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

func copyRequestHeaders(destination, source http.Header) {
	copyHeaders(destination, source)
	destination.Del("Host")
	destination.Del("Content-Length")
	destination.Del(accountHeader)
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
