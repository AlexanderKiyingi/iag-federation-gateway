// Package platformauth wraps the shared authclient verifier so the rest of the
// service depends on a local type.
package platformauth

import (
	"context"
	"time"

	"github.com/alvor-technologies/iag-platform-go/authclient"
)

type Claims = authclient.Claims

type Verifier struct {
	inner *authclient.Verifier
}

func NewVerifier(jwksURL, issuer, audience string) *Verifier {
	return &Verifier{inner: authclient.NewVerifier(authclient.Options{
		JWKSURL: jwksURL, Issuer: issuer, Audience: audience,
	})}
}

func (v *Verifier) Refresh(ctx context.Context) error { return v.inner.Refresh(ctx) }

func (v *Verifier) StartRefreshLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = v.inner.Refresh(ctx)
			}
		}
	}()
}

func (v *Verifier) Verify(token string) (*Claims, error) { return v.inner.Verify(token) }

// HasKeys reports whether any signing key is loaded. With an empty key set every
// token fails closed, so the refresh loop uses this to retry hard rather than
// waiting out a full rotation interval.
func (v *Verifier) HasKeys() bool { return v.inner.HasKeys() }

// Inner exposes the underlying shared verifier for platform middleware.
func (v *Verifier) Inner() *authclient.Verifier { return v.inner }
