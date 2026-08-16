package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/iag/federation-gateway/internal/config"
	"github.com/iag/federation-gateway/internal/handlers"
	"github.com/iag/federation-gateway/internal/middleware"
	"github.com/iag/federation-gateway/internal/models"
	"github.com/iag/federation-gateway/internal/platformauth"
)

func testRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Config{
		ServiceName:      "federation-gateway",
		Audience:         "iag.federation-gateway",
		ConflictStrategy: models.StrategyLastWriteWins,
		MaxPushBatch:     200,
		MaxPullBatch:     500,
	}
	// A verifier with no keys loaded: every token fails closed, which is the
	// behaviour under test for the protected routes.
	verifier := platformauth.NewVerifier("http://127.0.0.1:1/jwks", "http://127.0.0.1:1", cfg.Audience)
	return New(Options{
		Cfg:          cfg,
		API:          &handlers.API{Cfg: cfg},
		PlatformAuth: middleware.NewPlatformAuth(verifier),
	})
}

func TestProbePathsArePublic(t *testing.T) {
	r := testRouter(t)
	for _, path := range []string{"/", "/health", "/healthz"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 without a token", path, w.Code)
		}
	}
}

// Every /v1 route must reject an unauthenticated caller. A federation gateway
// that served node or conflict data anonymously would leak the shape of every
// federated record.
func TestV1RequiresAuth(t *testing.T) {
	r := testRouter(t)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/nodes"},
		{http.MethodGet, "/v1/nodes/node-a"},
		{http.MethodPost, "/v1/nodes/register"},
		{http.MethodPatch, "/v1/nodes/node-a/status"},
		{http.MethodPost, "/v1/sync/push"},
		{http.MethodGet, "/v1/sync/pull"},
		{http.MethodPost, "/v1/sync/ack"},
		{http.MethodGet, "/v1/resources/invoice/inv-1"},
		{http.MethodGet, "/v1/conflicts"},
		{http.MethodGet, "/v1/conflicts/abc"},
		{http.MethodPost, "/v1/conflicts/abc/resolve"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

func TestBearerWithUnloadableKeysIsRejected(t *testing.T) {
	r := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bogus token = %d, want 401", w.Code)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	r := testRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
}
