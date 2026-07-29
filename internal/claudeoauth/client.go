package claudeoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/credential"
)

const (
	ClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AuthorizeURL = "https://claude.ai/oauth/authorize"
	TokenURL     = "https://platform.claude.com/v1/oauth/token"
	RedirectURI  = "https://platform.claude.com/oauth/code/callback"
	Scope        = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	sessionTTL   = 30 * time.Minute
)

type Session struct {
	Alias        string
	State        string
	CodeVerifier string
	CreatedAt    time.Time
}

type StartResult struct {
	AuthorizationURL string `json:"authorization_url"`
	SessionID        string `json:"session_id"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type Client struct {
	httpClient   *http.Client
	authorizeURL string
	tokenURL     string
	redirectURI  string
	mu           sync.Mutex
	sessions     map[string]Session
}

func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		httpClient:   httpClient,
		authorizeURL: AuthorizeURL,
		tokenURL:     TokenURL,
		redirectURI:  RedirectURI,
		sessions:     make(map[string]Session),
	}
}

func NewForTest(httpClient *http.Client, authorizeURL, tokenURL, redirectURI string) *Client {
	client := New(httpClient)
	client.authorizeURL = authorizeURL
	client.tokenURL = tokenURL
	client.redirectURI = redirectURI
	return client
}

func (c *Client) Start(alias string) (StartResult, error) {
	state, err := randomURLToken(32)
	if err != nil {
		return StartResult{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return StartResult{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	sessionID, err := randomURLToken(18)
	if err != nil {
		return StartResult{}, fmt.Errorf("generate OAuth session: %w", err)
	}
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])

	values := url.Values{
		"code":                  {"true"},
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {c.redirectURI},
		"scope":                 {Scope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	c.mu.Lock()
	now := time.Now()
	for id, session := range c.sessions {
		if now.Sub(session.CreatedAt) > sessionTTL {
			delete(c.sessions, id)
		}
	}
	c.sessions[sessionID] = Session{Alias: alias, State: state, CodeVerifier: verifier, CreatedAt: now}
	c.mu.Unlock()
	return StartResult{
		AuthorizationURL: c.authorizeURL + "?" + values.Encode(),
		SessionID:        sessionID,
		ExpiresInSeconds: int64(sessionTTL / time.Second),
	}, nil
}

func (c *Client) Exchange(ctx context.Context, sessionID, suppliedCode string) (string, credential.Credential, error) {
	c.mu.Lock()
	session, found := c.sessions[sessionID]
	c.mu.Unlock()
	if !found || time.Since(session.CreatedAt) > sessionTTL {
		return "", credential.Credential{}, fmt.Errorf("OAuth session was not found or has expired")
	}
	code, returnedState, err := parseAuthorizationCode(suppliedCode)
	if err != nil {
		return "", credential.Credential{}, err
	}
	if returnedState != "" && returnedState != session.State {
		return "", credential.Credential{}, fmt.Errorf("OAuth state does not match")
	}
	payload := map[string]any{
		"code":          code,
		"state":         session.State,
		"grant_type":    "authorization_code",
		"client_id":     ClientID,
		"redirect_uri":  c.redirectURI,
		"code_verifier": session.CodeVerifier,
	}
	response, err := c.requestToken(ctx, payload)
	if err != nil {
		return "", credential.Credential{}, err
	}
	if response.AccessToken == "" || response.RefreshToken == "" {
		return "", credential.Credential{}, fmt.Errorf("OAuth token response omitted required tokens")
	}
	if response.ExpiresIn <= 0 {
		return "", credential.Credential{}, fmt.Errorf("OAuth token response has an invalid expiry")
	}
	accountUUID := strings.TrimSpace(response.Account.UUID)
	if accountUUID == "" {
		accountUUID = strings.TrimSpace(response.Organization.UUID)
	}
	cred, err := credential.Prepare(credential.Credential{
		Type:         "claude",
		AccessToken:  response.AccessToken,
		RefreshToken: response.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(response.ExpiresIn) * time.Second).Format(time.RFC3339),
		Email:        strings.TrimSpace(response.Account.EmailAddress),
		AccountUUID:  accountUUID,
		Extra: map[string]any{
			"scope":      response.Scope,
			"token_type": response.TokenType,
		},
	})
	if err != nil {
		return "", credential.Credential{}, fmt.Errorf("prepare OAuth credential: %w", err)
	}
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.mu.Unlock()
	return session.Alias, cred, nil
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (RefreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return RefreshResult{}, fmt.Errorf("refresh token is missing")
	}
	response, err := c.requestToken(ctx, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	if err != nil {
		return RefreshResult{}, err
	}
	if response.AccessToken == "" {
		return RefreshResult{}, fmt.Errorf("refresh response omitted access token")
	}
	if response.ExpiresIn <= 0 {
		return RefreshResult{}, fmt.Errorf("refresh response has an invalid expiry")
	}
	rotated := response.RefreshToken
	if rotated == "" {
		rotated = refreshToken
	}
	return RefreshResult{
		AccessToken:  response.AccessToken,
		RefreshToken: rotated,
		ExpiresAt:    time.Now().Add(time.Duration(response.ExpiresIn) * time.Second),
	}, nil
}

func (c *Client) requestToken(ctx context.Context, payload map[string]any) (tokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("encode OAuth token request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("create OAuth token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "axios/1.13.6")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("OAuth token request failed: %w", err)
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("read OAuth token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return tokenResponse{}, fmt.Errorf("OAuth token endpoint returned status %d", response.StatusCode)
	}
	var decoded tokenResponse
	if err := json.Unmarshal(limited, &decoded); err != nil {
		return tokenResponse{}, fmt.Errorf("decode OAuth token response: %w", err)
	}
	return decoded, nil
}

func parseAuthorizationCode(value string) (code, state string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", fmt.Errorf("authorization code is required")
	}
	if parsed, parseErr := url.Parse(value); parseErr == nil && parsed.IsAbs() {
		if queryCode := parsed.Query().Get("code"); queryCode != "" {
			value = queryCode
			state = parsed.Query().Get("state")
			if state == "" {
				state = parsed.Fragment
			}
		}
	}
	if before, after, found := strings.Cut(value, "#"); found {
		value = before
		if state == "" {
			state = after
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", fmt.Errorf("authorization code is required")
	}
	return value, strings.TrimSpace(state), nil
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
