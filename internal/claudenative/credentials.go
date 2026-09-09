package claudenative

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// keychainService is where Claude Code keeps the subscription OAuth
// credential on macOS, keyed by service name and the OS account.
const keychainService = "Claude Code-credentials"

// credentialsFile is the only login anchor a harness with an explicit
// CLAUDE_CONFIG_DIR reads.
const credentialsFile = ".credentials.json"

// seedCredentials makes the operator's subscription login reachable from the
// private config directory, and returns a function that removes it again.
//
// Setting CLAUDE_CONFIG_DIR is what makes containment possible, and it is also
// what stops Claude Code consulting the Keychain: with the variable set it
// reads only $CLAUDE_CONFIG_DIR/.credentials.json. Measured on 2.1.263 —
// seeding oauthAccount, userID, or the whole .claude.json does not help, and
// pointing the variable back at the real ~/.claude does not either.
//
// So the subscription costs one thing: the token exists as a file for the life
// of the session. It is written 0600 inside a directory this process created
// 0700 and holds an exclusive lock on, and removed when the session ends. A
// SIGKILL or a power cut leaves it behind — that is the accepted cost, not an
// oversight.
//
// An API key in the environment skips all of this; seeding only happens when
// there is no key to use.
func seedCredentials(configDir string) (func(), error) {
	path := filepath.Join(configDir, credentialsFile)
	if _, err := os.Stat(path); err == nil {
		// Already present: the operator manages it, so leave it alone rather
		// than overwrite it and delete it out from under them on exit.
		return func() {}, nil
	}
	secret, err := readKeychainCredential()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, fmt.Errorf("seed credentials: %w", err)
	}
	return func() { os.Remove(path) }, nil
}

// readKeychainCredential returns the stored credential, verified to be the
// JSON document Claude Code expects. A truncated or foreign value would
// otherwise reach the harness as an unexplained login failure.
func readKeychainCredential() ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("no credential source: set ANTHROPIC_API_KEY, " +
			"or place .credentials.json in the runtime home's config directory")
	}
	out, err := exec.Command("security", "find-generic-password",
		"-w", "-s", keychainService).Output()
	if err != nil {
		return nil, fmt.Errorf("no Claude Code credential in the keychain "+
			"(log in once with claude, or set ANTHROPIC_API_KEY): %w", err)
	}
	secret := []byte(strings.TrimRight(string(out), "\n"))
	if err := validateCredential(secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// validateCredential checks the stored value is the document Claude Code
// expects. A truncated or foreign secret would otherwise be written out and
// surface as an unexplained login failure inside the harness, where the
// operator has no way to see what went wrong.
func validateCredential(secret []byte) error {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(secret, &parsed); err != nil {
		return fmt.Errorf("keychain credential is not JSON: %w", err)
	}
	if _, ok := parsed["claudeAiOauth"]; !ok {
		return errors.New("keychain credential has no claudeAiOauth field")
	}
	return nil
}

// hasAPIKey reports whether the environment already carries an API key, in
// which case the subscription credential is not needed.
func hasAPIKey() bool {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}
