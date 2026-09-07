package codexnative

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoveychen/remote-adapter/internal/trustedstate"
)

func TestNativeServiceBindsActualThreadAndReplaysReceipt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := trustedstate.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	token := strings.Repeat("n", 64)
	installerToken := strings.Repeat("i", 64)
	handler, bind, storeID, err := nativeService(state, token, strings.Repeat("b", 64), installerToken)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, auth string, body any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/native/v2/"+path, bytes.NewReader(encoded))
		req.Header.Set("Authorization", "Bearer "+auth)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		return result
	}
	handshake := request("handshake", token, map[string]any{})
	if handshake.Code != 200 || !strings.Contains(handshake.Body.String(), storeID) {
		t.Fatal(handshake)
	}
	installerHandshake := request("handshake", installerToken, map[string]any{})
	if installerHandshake.Code != 200 || !strings.Contains(installerHandshake.Body.String(), "native_skills_packages") {
		t.Fatal(installerHandshake)
	}
	note := map[string]any{"call_id": "call-1", "filename": "2026-09-06T01-02-03-native.md", "note": "canonical note"}
	if got := request("memory.note.create", token, note); got.Code != 503 {
		t.Fatal(got)
	}
	if err := bind("actual-thread-id"); err != nil {
		t.Fatal(err)
	}
	if err := bind("different-thread"); err == nil {
		t.Fatal("rebound credential")
	}
	first := request("memory.note.create", token, note)
	second := request("memory.note.create", token, note)
	if first.Code != 200 || second.Code != 200 || first.Header().Get("X-RCA-Commit-Sequence") == "" || first.Header().Get("X-RCA-Commit-Sequence") != second.Header().Get("X-RCA-Commit-Sequence") {
		t.Fatal(first, second)
	}
	if got := request("memory.list", strings.Repeat("m", 64), map[string]any{}); got.Code != 401 {
		t.Fatal(got)
	}
	journal, err := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), "bootstrap") || strings.Count(string(journal), `"operation":"memory.note.create"`) != 1 || !strings.Contains(string(journal), `"thread_id":"actual-thread-id"`) {
		t.Fatal(string(journal))
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if got := request("handshake", token, map[string]any{}); got.Code != 503 {
		t.Fatal(got)
	}
}
