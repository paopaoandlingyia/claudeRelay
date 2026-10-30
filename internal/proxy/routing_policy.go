package proxy

import (
	"fmt"
	"strings"
	"sync/atomic"
)

const (
	routingPolicyLegacy     = "legacy"
	routingPolicyQuotaAware = "quota_aware"
)

// Routing policy is process-local. NewServer starts with quota-aware routing;
// operators can temporarily switch to legacy affinity routing.
type routingPolicyState struct {
	quotaAware atomic.Bool
}

func (s *routingPolicyState) current() string {
	if s != nil && s.quotaAware.Load() {
		return routingPolicyQuotaAware
	}
	return routingPolicyLegacy
}

func (s *routingPolicyState) set(raw string) (string, error) {
	policy := strings.ToLower(strings.TrimSpace(raw))
	switch policy {
	case routingPolicyLegacy:
		s.quotaAware.Store(false)
	case routingPolicyQuotaAware:
		s.quotaAware.Store(true)
	default:
		return "", fmt.Errorf("routing policy must be %q or %q", routingPolicyLegacy, routingPolicyQuotaAware)
	}
	return policy, nil
}
