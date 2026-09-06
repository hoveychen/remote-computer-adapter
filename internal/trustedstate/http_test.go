package trustedstate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPAuthAndFraming(t *testing.T) {
	s, _ := openTest(t)
	h := HTTPHandler(s, "secret")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_put","arguments":{"id":"x","content":"one","expected_revision":0,"request_id":"q"}}}`
	for _, tc := range []struct {
		auth, origin, body string
		status             int
	}{{"", "", body, 401}, {"Bearer wrong", "", body, 401}, {"Bearer secret", "https://evil.example", body, 403}, {"Bearer secret", "", body + "\n" + body, 400}, {"Bearer secret", "", body, 200}} {
		r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(tc.body))
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if s.sequence != 1 {
		t.Fatal("unauthorized mutation", s.sequence)
	}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/mcp", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
