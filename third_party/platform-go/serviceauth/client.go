// Package serviceauth fetches and caches OAuth2 client_credentials access
// tokens for outbound service-to-service calls. Replaces the static
// GATEWAY_INTERNAL_SECRET trust pattern.
package serviceauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Options configures a Client.
type Options struct {
	// TokenURL is the OAuth2 token endpoint, e.g. http://authentication:3001/oauth/token
	TokenURL string
	// ClientID identifies this service to the auth server (e.g. "iag-notifications").
	ClientID string
	// ClientSecret is the OAuth2 client secret.
	ClientSecret string
	// Audience is the upstream service this token will be presented to (e.g. "iag.finance").
	// If empty, no audience is requested.
	Audience string
	// EarlyRefresh is how long before expiry to proactively re-issue. Defaults to 60s.
	EarlyRefresh time.Duration
	// HTTPClient overrides the default. Optional.
	HTTPClient *http.Client
}

// Client caches a client_credentials access token and refreshes it before expiry.
type Client struct {
	opts   Options
	http   *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewClient builds a Client. The first Token() call performs the initial fetch.
func NewClient(opts Options) *Client {
	if opts.TokenURL == "" || opts.ClientID == "" || opts.ClientSecret == "" {
		panic("serviceauth: TokenURL, ClientID, and ClientSecret are required")
	}
	if opts.EarlyRefresh <= 0 {
		opts.EarlyRefresh = 60 * time.Second
	}
	httpC := opts.HTTPClient
	if httpC == nil {
		httpC = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{opts: opts, http: httpC}
}

// Token returns a valid access token, refreshing if needed.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiry) > c.opts.EarlyRefresh {
		return c.token, nil
	}
	if err := c.fetchLocked(ctx); err != nil {
		return "", err
	}
	return c.token, nil
}

// AuthorizeRequest attaches a fresh Bearer token to the request.
func (c *Client) AuthorizeRequest(ctx context.Context, req *http.Request) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (c *Client) fetchLocked(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.opts.ClientID)
	form.Set("client_secret", c.opts.ClientSecret)
	if c.opts.Audience != "" {
		form.Set("audience", c.opts.Audience)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("serviceauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("serviceauth: post token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("serviceauth: token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("serviceauth: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return errors.New("serviceauth: empty access_token in response")
	}
	c.token = tr.AccessToken
	c.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return nil
}
