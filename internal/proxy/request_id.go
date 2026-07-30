package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const requestIDHeader = "X-Claude-Relay-Request-ID"

type requestIDContextKey struct{}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := validRequestID(r.Header.Get(requestIDHeader))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set(requestIDHeader, requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

func validRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return ""
	}
	return value
}

func newRequestID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	return fmt.Sprintf("fallback-%x", time.Now().UnixNano())
}
