package apierr

import (
	"encoding/json"
	"net/http"
)

// WriteHTTP writes the standard error envelope to an http.ResponseWriter.
func WriteHTTP(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body(code, message))
}
