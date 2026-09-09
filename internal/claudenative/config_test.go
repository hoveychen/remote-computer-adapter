package claudenative

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	base := t.TempDir()
	return Config{
		Binary:      filepath.Join(base, "claude"),
		RuntimeHome: filepath.Join(base, "runtime"),
		StateRoot:   filepath.Join(base, "state"),
		RemoteRoot:  "/work/project",
		ExecProgram: "/usr/bin/ssh",
		ExecArgs:    []string{"-T", "host", "rca serve --root /work/project"},
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	good, err := json.Marshal(validConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"unknown field": `{"binary":"/bin/claude","local":true}`,
		"two objects":   string(good) + string(good),
		"not an object": `[]`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestValidateRequiresAbsolutePaths(t *testing.T) {
	for field, mutate := range map[string]func(*Config){
		"binary":       func(c *Config) { c.Binary = "claude" },
		"runtime_home": func(c *Config) { c.RuntimeHome = "runtime" },
		"state_root":   func(c *Config) { c.StateRoot = "state" },
		"remote_root":  func(c *Config) { c.RemoteRoot = "project" },
		"exec_program": func(c *Config) { c.ExecProgram = "ssh" },
	} {
		c := validConfig(t)
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("relative %s was accepted", field)
		}
	}
	// A newline in a path would let a config value forge extra lines wherever
	// the value is later rendered into a line-oriented format.
	c := validConfig(t)
	c.RemoteRoot = "/work\nmalicious"
	if err := c.validate(); err == nil {
		t.Error("a path containing a newline was accepted")
	}
}

func TestValidateRejectsSharedStateAndRuntimeDir(t *testing.T) {
	c := validConfig(t)
	c.StateRoot = c.RuntimeHome
	if err := c.validate(); err == nil {
		t.Error("runtime_home == state_root was accepted")
	}
}

func TestValidateArgsTakesOnlyAPrompt(t *testing.T) {
	if err := ValidateArgs([]string{"fix the test"}); err != nil {
		t.Fatalf("a plain prompt was rejected: %v", err)
	}
	for _, args := range [][]string{
		{},
		{"one", "two"},
		{"--dangerously-skip-permissions"},
		{"--tools", "Bash"},
	} {
		if err := ValidateArgs(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

// The launch flags are the whole boundary; a regression here is silent.
func TestHarnessArgsSealTheToolSurface(t *testing.T) {
	args := harnessArgs(validConfig(t), `{"mcpServers":{}}`, []string{"mcp__rca__memory_read"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--tools", "--restricted", "--strict-mcp-config",
		"--disable-slash-commands", "--permission-prompts none"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	// --tools must be followed by the empty string, not by a tool name.
	i := slices.Index(args, "--tools")
	if i < 0 || i+1 >= len(args) || args[i+1] != "" {
		t.Fatalf("--tools is not followed by an empty value: %v", args)
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("permission checks are being skipped")
	}
}

func TestHarnessArgsOmitsModelWhenUnset(t *testing.T) {
	if slices.Contains(harnessArgs(validConfig(t), "{}", nil), "--model") {
		t.Error("--model was passed with no model configured")
	}
	c := validConfig(t)
	c.Model = "claude-opus-5"
	args := harnessArgs(c, "{}", nil)
	i := slices.Index(args, "--model")
	if i < 0 || args[i+1] != "claude-opus-5" {
		t.Errorf("--model not passed through: %v", args)
	}
}

// The harness environment is built, not inherited: an inherited variable is a
// channel nobody chose, and credentials are exactly what must not spread.
func TestHarnessEnvIsExplicit(t *testing.T) {
	t.Setenv("RCA_UNRELATED_CANARY", "leaked")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	c := validConfig(t)
	env := harnessEnv(c, "state-token")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "RCA_UNRELATED_CANARY") {
		t.Error("an unrelated variable was inherited")
	}
	for _, want := range []string{
		"HOME=" + filepath.Join(c.RuntimeHome, "user-home"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(c.RuntimeHome, "config"),
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"RCA_STATE_TOKEN=state-token",
		"ANTHROPIC_API_KEY=sk-test",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestMCPConfigCarriesTheBearerToken(t *testing.T) {
	body, err := mcpConfig("http://127.0.0.1:1234/mcp", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	server, ok := parsed.MCPServers["rca"]
	if !ok {
		t.Fatal("no rca server in the config")
	}
	if server.Type != "http" || server.URL != "http://127.0.0.1:1234/mcp" {
		t.Errorf("server = %+v", server)
	}
	if server.Headers["Authorization"] != "Bearer secret-token" {
		t.Errorf("authorization = %q", server.Headers["Authorization"])
	}
}
