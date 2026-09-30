package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/local/claude-relay/internal/store"
)

const officialVersionBoundsSetting = "official_cli_version_bounds"

var claudeCodeUAVersionPattern = regexp.MustCompile(`(?i)^claude-cli/(\d+)\.(\d+)\.(\d+)`)
var configuredVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

type officialVersionBounds struct {
	Min string `json:"min_version"`
	Max string `json:"max_version"`
}

type officialVersionPolicy struct {
	mu     sync.RWMutex
	bounds officialVersionBounds
}

func loadOfficialVersionPolicy(database *store.Store) (*officialVersionPolicy, error) {
	value, found, err := database.RuntimeSetting(context.Background(), officialVersionBoundsSetting)
	if err != nil {
		return nil, err
	}
	policy := &officialVersionPolicy{}
	if !found || strings.TrimSpace(value) == "" {
		return policy, nil
	}
	var bounds officialVersionBounds
	if err := json.Unmarshal([]byte(value), &bounds); err != nil {
		return nil, fmt.Errorf("decode persisted official CLI version bounds: %w", err)
	}
	if err := validateOfficialVersionBounds(bounds.Min, bounds.Max); err != nil {
		return nil, fmt.Errorf("validate persisted official CLI version bounds: %w", err)
	}
	policy.bounds = officialVersionBounds{Min: strings.TrimSpace(bounds.Min), Max: strings.TrimSpace(bounds.Max)}
	return policy, nil
}

func (p *officialVersionPolicy) current() officialVersionBounds {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bounds
}

func (p *officialVersionPolicy) set(bounds officialVersionBounds) {
	p.mu.Lock()
	p.bounds = bounds
	p.mu.Unlock()
}

func (p *officialVersionPolicy) persist(ctx context.Context, database *store.Store, bounds officialVersionBounds) error {
	encoded, err := json.Marshal(bounds)
	if err != nil {
		return fmt.Errorf("encode official CLI version bounds: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := database.SetRuntimeSetting(ctx, officialVersionBoundsSetting, string(encoded)); err != nil {
		return err
	}
	p.bounds = bounds
	return nil
}

func (p *officialVersionPolicy) check(userAgent string) error {
	bounds := p.current()
	if bounds.Min == "" && bounds.Max == "" {
		return nil
	}
	version := extractClaudeCodeUAVersion(userAgent)
	if version == "" {
		return fmt.Errorf("official ingress requires a versioned Claude Code User-Agent")
	}
	if bounds.Min != "" && compareSemanticVersions(version, bounds.Min) < 0 {
		return fmt.Errorf("Claude Code version %s is below the configured minimum %s", version, bounds.Min)
	}
	if bounds.Max != "" && compareSemanticVersions(version, bounds.Max) > 0 {
		return fmt.Errorf("Claude Code version %s exceeds the configured maximum %s", version, bounds.Max)
	}
	return nil
}

func extractClaudeCodeUAVersion(userAgent string) string {
	matches := claudeCodeUAVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 4 {
		return ""
	}
	return matches[1] + "." + matches[2] + "." + matches[3]
}

func validateOfficialVersionBounds(minimum, maximum string) error {
	minimum = strings.TrimSpace(minimum)
	maximum = strings.TrimSpace(maximum)
	if minimum != "" && !configuredVersionPattern.MatchString(minimum) {
		return fmt.Errorf("minimum version must be empty or semver x.y.z")
	}
	if maximum != "" && !configuredVersionPattern.MatchString(maximum) {
		return fmt.Errorf("maximum version must be empty or semver x.y.z")
	}
	if minimum != "" && maximum != "" && compareSemanticVersions(minimum, maximum) > 0 {
		return fmt.Errorf("maximum version must be greater than or equal to minimum version")
	}
	return nil
}

func compareSemanticVersions(left, right string) int {
	for i, leftPart := range strings.Split(left, ".") {
		leftValue, _ := strconv.Atoi(leftPart)
		rightValue, _ := strconv.Atoi(strings.Split(right, ".")[i])
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}
