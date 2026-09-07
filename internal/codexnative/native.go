package codexnative

import (
	"errors"
	"net/http"
	"sync"

	"github.com/hoveychen/remote-adapter/internal/trustedstate"
)

// nativeService binds writes only after app-server supplies its actual thread ID.
// Bootstrap can read the summary, but cannot create notes under a synthetic ID.
func nativeService(state *trustedstate.Store, modelToken, backgroundToken string) (http.Handler, func(string) error, string, error) {
	initial, err := trustedstate.NativeHTTPHandler(state, []trustedstate.NativeCredential{{Token: modelToken, Kind: "model_tool", ThreadID: "bootstrap"}, {Token: backgroundToken, Kind: "background", ThreadID: "background"}})
	if err != nil {
		return nil, nil, "", err
	}
	identity, err := state.NativeRead("maintenance", "store-id", 0)
	if err != nil {
		return nil, nil, "", err
	}
	var mu sync.RWMutex
	current := initial
	bound := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !bound && r.URL.Path == "/native/v2/memory.note.create" {
			http.Error(w, "thread not bound", http.StatusServiceUnavailable)
			return
		}
		current.ServeHTTP(w, r)
	})
	bind := func(threadID string) error {
		mu.Lock()
		defer mu.Unlock()
		if bound {
			return errors.New("native thread already bound")
		}
		next, err := trustedstate.NativeHTTPHandler(state, []trustedstate.NativeCredential{{Token: modelToken, Kind: "model_tool", ThreadID: threadID}, {Token: backgroundToken, Kind: "background", ThreadID: "background"}})
		if err != nil {
			return err
		}
		current, bound = next, true
		return nil
	}
	return handler, bind, string(identity.Content), nil
}

func nativeMemoryConfig(endpoint, storeID string) string {
	return "\n[memories.native_service]\nendpoint = " + quote(endpoint) + "\nbearer_token_env_var = \"RCA_NATIVE_MEMORY_TOKEN\"\nbackground_bearer_token_env_var = \"RCA_NATIVE_MEMORY_BACKGROUND_TOKEN\"\nstore_id = " + quote(storeID) + "\n"
}
