package httplisten

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ResolvePort returns the TCP port the HTTP server should bind to.
// Precedence: PORT (Railway/PaaS), HTTP_PORT (explicit service port), defaultPort.
func ResolvePort(defaultPort int) (port int, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		p, parseErr := parsePort(v)
		if parseErr != nil {
			return 0, "PORT", fmt.Errorf("invalid PORT %q: %w", v, parseErr)
		}
		return p, "PORT", nil
	}
	if v := strings.TrimSpace(os.Getenv("HTTP_PORT")); v != "" {
		p, parseErr := parsePort(v)
		if parseErr != nil {
			return 0, "HTTP_PORT", fmt.Errorf("invalid HTTP_PORT %q: %w", v, parseErr)
		}
		return p, "HTTP_PORT", nil
	}
	return defaultPort, "default", nil
}

func parsePort(v string) (int, error) {
	v = strings.TrimPrefix(v, ":")
	p, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("port out of range")
	}
	return p, nil
}
