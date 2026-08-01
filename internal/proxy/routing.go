package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	accountHeader    = "X-Claude-Relay-Account"
	stickySessionTTL = time.Hour
)

type requestRoute struct {
	ConversationKey string
	SelectionKey    string
	AccountUUID     string
	Model           string
	AccountPool     string
	Client          clientObservation
}

type metadataIdentity struct {
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func deriveRequestRoute(body []byte, headers http.Header, accountPool string) (requestRoute, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return requestRoute{}, fmt.Errorf("decode request body: %w", err)
	}
	if root == nil {
		return requestRoute{}, fmt.Errorf("request body must be a JSON object")
	}
	route := requestRoute{AccountPool: accountPool}
	route.Model, _ = root["model"].(string)
	route.Client = classifyClient(root, headers)

	identity := parseMetadataIdentity(root["metadata"])
	route.AccountUUID = identity.AccountUUID
	session := firstSessionHeader(headers)
	if session == "" {
		session = identity.SessionID
	}
	scope := shortHash(accountPool)
	if session != "" {
		route.ConversationKey = "session:" + shortHash(scope+"\x00"+session)
	}

	prefix := cachePrefix(root)
	if prefix == nil {
		prefix = ordinaryAnchor(root)
	}
	raw, err := json.Marshal(prefix)
	if err != nil {
		return requestRoute{}, fmt.Errorf("encode routing prefix: %w", err)
	}
	route.SelectionKey = "prefix:" + shortHash(scope+"\x00"+route.Model+"\x00"+string(raw))
	if route.ConversationKey == "" {
		route.ConversationKey = route.SelectionKey
	}
	return route, nil
}

func parseMetadataIdentity(value any) metadataIdentity {
	metadata, ok := value.(map[string]any)
	if !ok {
		return metadataIdentity{}
	}
	userID, _ := metadata["user_id"].(string)
	var identity metadataIdentity
	_ = json.Unmarshal([]byte(userID), &identity)
	return identity
}

func firstSessionHeader(headers http.Header) string {
	for _, name := range sessionHeaderNames {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func cachePrefix(root map[string]any) any {
	tools, _ := root["tools"].([]any)
	system := normalizeRoutingBlocks(root["system"])
	messages, _ := root["messages"].([]any)

	if prefix, ok := messagePrefixThroughCacheControl(messages); ok {
		return map[string]any{"tools": tools, "system": system, "messages": prefix}
	}
	if prefix, ok := blocksThroughCacheControl(system); ok {
		return map[string]any{"tools": tools, "system": prefix}
	}
	if prefix, ok := blocksThroughCacheControl(tools); ok {
		return map[string]any{"tools": prefix}
	}
	return nil
}

func ordinaryAnchor(root map[string]any) any {
	anchor := map[string]any{
		"tools":  root["tools"],
		"system": root["system"],
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || message["role"] != "user" {
				continue
			}
			anchor["first_user"] = message["content"]
			break
		}
	}
	return anchor
}

func normalizeRoutingBlocks(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case string:
		return []any{typed}
	default:
		return nil
	}
}

func blocksThroughCacheControl(blocks []any) ([]any, bool) {
	last := -1
	for i, block := range blocks {
		if hasCacheControl(block) {
			last = i
		}
	}
	if last < 0 {
		return nil, false
	}
	return blocks[:last+1], true
}

func messagePrefixThroughCacheControl(messages []any) ([]any, bool) {
	lastMessage, lastBlock := -1, -1
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			if hasCacheControl(message["content"]) {
				lastMessage, lastBlock = i, -1
			}
			continue
		}
		for j, block := range content {
			if hasCacheControl(block) {
				lastMessage, lastBlock = i, j
			}
		}
	}
	if lastMessage < 0 {
		return nil, false
	}
	prefix := append([]any(nil), messages[:lastMessage+1]...)
	if lastBlock >= 0 {
		message := cloneMap(messages[lastMessage].(map[string]any))
		content := message["content"].([]any)
		message["content"] = content[:lastBlock+1]
		prefix[lastMessage] = message
	}
	return prefix, true
}

func cloneMap(source map[string]any) map[string]any {
	destination := make(map[string]any, len(source))
	for key, value := range source {
		destination[key] = value
	}
	return destination
}

func hasCacheControl(value any) bool {
	block, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, exists := block["cache_control"]
	return exists
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

type selection struct {
	Account store.Account
	Pinned  bool
	Source  string
}

type accountSelector struct{ store *store.Store }

func (s accountSelector) selectAccount(ctx context.Context, route requestRoute, forcedAlias string, excluded map[int64]bool) (selection, error) {
	forcedAlias = strings.TrimSpace(forcedAlias)
	if forcedAlias != "" {
		account, found, err := s.store.AccountByAlias(ctx, forcedAlias)
		if err != nil {
			return selection{}, err
		}
		if !found || !account.Enabled {
			return selection{}, fmt.Errorf("requested account %q is unavailable", forcedAlias)
		}
		if account.Pool != route.AccountPool {
			return selection{}, fmt.Errorf("requested account %q is outside the %s pool", forcedAlias, route.AccountPool)
		}
		cooling, err := s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
		if err != nil {
			return selection{}, err
		}
		if cooling {
			return selection{}, fmt.Errorf("requested account %q is temporarily cooling down", forcedAlias)
		}
		return selection{Account: account, Pinned: true, Source: "header"}, nil
	}

	if route.AccountUUID != "" {
		account, found, err := s.store.AccountByUUID(ctx, route.AccountUUID, route.AccountPool)
		if err != nil {
			return selection{}, err
		}
		cooling := false
		if found {
			cooling, err = s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
			if err != nil {
				return selection{}, err
			}
		}
		if found && !cooling && !excluded[account.ID] {
			return selection{Account: account, Source: "account_uuid"}, nil
		}
	}

	if route.ConversationKey != "" {
		account, found, err := s.store.BoundAccount(ctx, route.ConversationKey, route.AccountPool, time.Now())
		if err != nil {
			return selection{}, err
		}
		cooling := false
		if found {
			cooling, err = s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
			if err != nil {
				return selection{}, err
			}
		}
		if found && !cooling && !excluded[account.ID] {
			return selection{Account: account, Source: "sticky"}, nil
		}
	}

	accounts, err := s.store.Accounts(ctx, route.AccountPool, route.Model, time.Now())
	if err != nil {
		return selection{}, err
	}
	var chosen *store.Account
	var chosenScore [32]byte
	for i := range accounts {
		account := accounts[i]
		if excluded[account.ID] {
			continue
		}
		score := sha256.Sum256([]byte(route.SelectionKey + "\x00" + strings.ToLower(account.Alias)))
		if chosen == nil || bytes.Compare(score[:], chosenScore[:]) > 0 {
			chosen, chosenScore = &account, score
		}
	}
	if chosen == nil {
		return selection{}, fmt.Errorf("no healthy Claude subscription account is available in the %s pool", route.AccountPool)
	}
	return selection{Account: *chosen, Source: "cache_affinity"}, nil
}
