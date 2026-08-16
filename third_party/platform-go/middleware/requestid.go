package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HeaderRequestID is the canonical correlation header for cross-service tracing.
const HeaderRequestID = "X-Request-Id"

// RequestID ensures every request has an X-Request-Id, either propagated from
// the caller or freshly generated. The id is mirrored on the response.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("iag.requestID", id)
		c.Writer.Header().Set(HeaderRequestID, id)
		c.Next()
	}
}

// RequestIDFrom reads the request id off a gin context.
func RequestIDFrom(c *gin.Context) string {
	v, ok := c.Get("iag.requestID")
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
