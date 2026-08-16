package authclient

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Options configures a Verifier.
type Options struct {
	JWKSURL  string
	Issuer   string
	Audience string

	// HTTPClient overrides the default JWKS fetcher. Optional.
	HTTPClient *http.Client
}

// Verifier validates RS256 access tokens against the live JWKS.
// It supports multi-key rotation by matching tokens to the JWKS entry whose
// kid the token header carries.
type Verifier struct {
	opts   Options
	http   *http.Client
	mu     sync.RWMutex
	keys   map[string]*rsa.PublicKey
	parser *jwt.Parser
}

// NewVerifier constructs a Verifier. Audience is required — services MUST
// declare which audience they accept, and tokens lacking it are rejected.
func NewVerifier(opts Options) *Verifier {
	if opts.Audience == "" {
		panic("authclient: Audience is required")
	}
	httpC := opts.HTTPClient
	if httpC == nil {
		httpC = &http.Client{Timeout: 5 * time.Second}
	}
	return &Verifier{
		opts: opts,
		http: httpC,
		keys: map[string]*rsa.PublicKey{},
		parser: jwt.NewParser(
			jwt.WithIssuer(opts.Issuer),
			jwt.WithAudience(opts.Audience),
			jwt.WithValidMethods([]string{"RS256"}),
			jwt.WithExpirationRequired(),
		),
	}
}

type jwkDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Refresh fetches the JWKS and replaces the in-memory key set atomically.
func (v *Verifier) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.opts.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("authclient: new jwks request: %w", err)
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return fmt.Errorf("authclient: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authclient: jwks status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("authclient: read jwks: %w", err)
	}

	var doc jwkDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("authclient: decode jwks: %w", err)
	}

	next := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		next[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: e,
		}
	}
	if len(next) == 0 {
		return errors.New("authclient: jwks contained no usable RSA keys")
	}

	v.mu.Lock()
	v.keys = next
	v.mu.Unlock()
	slog.Debug("jwks refreshed", "kids", keysOf(next), "audience", v.opts.Audience)
	return nil
}

// StartRefreshLoop runs Refresh on every interval until ctx is cancelled.
func (v *Verifier) StartRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := v.Refresh(ctx); err != nil {
					slog.Warn("jwks refresh failed", "error", err)
				}
			}
		}
	}()
}

// Verify parses and validates a Bearer token. The token must:
//   - be RS256
//   - carry a kid header matching a JWKS entry
//   - carry iss == Options.Issuer
//   - carry aud containing Options.Audience
//   - not be expired
func (v *Verifier) Verify(token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := v.parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("authclient: token missing kid header")
		}
		v.mu.RLock()
		key, ok := v.keys[kid]
		v.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("authclient: unknown kid %q", kid)
		}
		return key, nil
	})
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("authclient: invalid token")
	}
	return claims, nil
}

// Audience returns the audience this verifier enforces.
func (v *Verifier) Audience() string { return v.opts.Audience }

// HasKeys reports whether the verifier currently holds at least one signing
// key. With an empty key set every token fails closed with "unknown kid", so
// callers use this to tell "auth is degraded, retry the JWKS fetch hard" apart
// from the healthy steady state.
func (v *Verifier) HasKeys() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.keys) > 0
}

func keysOf(m map[string]*rsa.PublicKey) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
