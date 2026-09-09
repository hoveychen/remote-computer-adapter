package claudenative

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateCredentialRejectsUnusableSecrets(t *testing.T) {
	for name, secret := range map[string]string{
		"empty":          ``,
		"truncated JSON": `{"claudeAiOauth":`,
		"not an object":  `"a-bare-string"`,
		"wrong document": `{"someOtherService":{"token":"x"}}`,
		"plain API key":  `sk-ant-not-a-json-document`,
	} {
		if err := validateCredential([]byte(secret)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := validateCredential([]byte(`{"claudeAiOauth":{"accessToken":"x"},"organizationUuid":"y"}`)); err != nil {
		t.Errorf("a well-formed credential was rejected: %v", err)
	}
}

// An existing credentials file belongs to the operator. Overwriting it would
// be bad enough; deleting it on exit would take their login with it.
func TestSeedCredentialsLeavesAnExistingFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsFile)
	original := []byte(`{"claudeAiOauth":{"accessToken":"operator-managed"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup, err := seedCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != string(original) {
		t.Fatalf("the existing credential was overwritten: %s", body)
	}
	cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cleanup deleted a file it did not create")
	}
}

// The seeded file is the whole cost of using a subscription: it must be
// unreadable to anyone else while it exists, and gone once the session ends.
func TestSeededCredentialIsPrivateAndRemoved(t *testing.T) {
	if _, err := readKeychainCredential(); err != nil {
		t.Skipf("no usable keychain credential on this host: %v", err)
	}
	dir := t.TempDir()
	cleanup, err := seedCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, credentialsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %o, want 600", perm)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the seeded credential outlived the session")
	}
}

func TestHasAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	if hasAPIKey() {
		t.Error("reported a key with none set")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-something")
	if !hasAPIKey() {
		t.Error("missed ANTHROPIC_AUTH_TOKEN")
	}
}
