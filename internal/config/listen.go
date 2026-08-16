package config

import (
	"os"
	"strings"
)

// ListenAddr resolves the bind address from PORT (Railway) or ADDR, defaulting
// to :4021 — the reserved edge-service port range (4020 is dms-depot-node).
func ListenAddr() string {
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		return normalizeListenAddr(p)
	}
	if v := strings.TrimSpace(os.Getenv("ADDR")); v != "" {
		return normalizeListenAddr(v)
	}
	return ":4021"
}

func normalizeListenAddr(addr string) string {
	if !strings.HasPrefix(addr, ":") && !strings.Contains(addr, ":") {
		return ":" + addr
	}
	return addr
}
