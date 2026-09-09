package trustedstate

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

// HTTPHandler implements stateless Streamable HTTP MCP. The owner must bind
// loopback; a fresh bearer token is held by the trusted harness, never executor.
// No cookies, CORS, sessions, GET event streams, or server-initiated calls.
func HTTPHandler(s ToolSet, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*MaxContent))
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		// A single JSON value per HTTP request, no batches or NDJSON mutations.
		var value map[string]any
		if err = strictJSON(b, &value); err != nil || value == nil {
			http.Error(w, "invalid JSON request", http.StatusBadRequest)
			return
		}
		compact := new(bytes.Buffer)
		// Re-encode via compact to permit pretty-printed JSON while retaining the
		// original argument number lexemes for integer validation.
		if err = json.Compact(compact, b); err != nil {
			http.Error(w, "invalid JSON request", 400)
			return
		}
		var out bytes.Buffer
		if err = serve(bytes.NewReader(append(compact.Bytes(), '\n')), &out, s, true); err != nil {
			http.Error(w, "MCP failure", 500)
			return
		}
		if out.Len() == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out.Bytes())
	})
}
