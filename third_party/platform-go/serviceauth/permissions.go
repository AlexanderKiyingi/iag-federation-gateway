package serviceauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Permission is one row in a service's permission registration payload. It
// mirrors the auth service's repository.PermissionInput; kept here so callers
// don't need to import auth-internal types.
type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// RegisterPermissions POSTs the supplied catalogue to the authentication
// service's /v1/permissions/register endpoint, using a service-account token
// minted from c. The endpoint is idempotent and additive — repeat calls only
// upsert; they never delete permissions absent from the payload.
//
// authBaseURL is the authentication service's external URL
// (e.g. "http://authentication:3001"). The path /v1/permissions/register is
// appended automatically.
//
// The Client c must already be configured with Audience = "iag.authentication"
// since that endpoint enforces aud=iag.authentication.
func RegisterPermissions(ctx context.Context, c *Client, authBaseURL, service string, perms []Permission) error {
	if c == nil {
		return fmt.Errorf("serviceauth: nil Client")
	}
	if service == "" {
		return fmt.Errorf("serviceauth: service is required")
	}
	if len(perms) == 0 {
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"service":     service,
		"permissions": perms,
	})
	if err != nil {
		return fmt.Errorf("serviceauth: marshal payload: %w", err)
	}

	endpoint := strings.TrimRight(authBaseURL, "/") + "/v1/permissions/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("serviceauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := c.AuthorizeRequest(ctx, req); err != nil {
		return fmt.Errorf("serviceauth: attach token: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("serviceauth: post register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("serviceauth: register status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}
