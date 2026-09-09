// Package claudenative launches Claude Code as a trusted harness.
//
// It cannot be native the way the Codex path is. Codex exposes a memory/skills
// backend the trusted service can be injected behind, so its native tools keep
// their names while their storage moves; Claude Code is a closed binary with no
// such seam. What is achievable is containment: Go owns the launch and the
// config, the built-in tool set is removed outright, and the only tools the
// model is offered are ones this process implements — general file and exec
// work bound to the remote executor, memory and skills bound to the trusted
// state service.
//
// The tool names the model sees therefore carry an mcp__ prefix. That is a
// real cost, and it is the one the syscall-interception design existed to
// avoid; it is accepted here because the alternative is no boundary at all.
package claudenative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is the trusted launch description. Every path is absolute and comes
// from the operator, never from the model.
type Config struct {
	// Binary is the claude executable.
	Binary string `json:"binary"`
	// RuntimeHome is the private directory that becomes HOME and
	// CLAUDE_CONFIG_DIR. It must be new or already owned by rca.
	RuntimeHome string `json:"runtime_home"`
	// StateRoot is the trusted state service's store.
	StateRoot string `json:"state_root"`
	// RemoteRoot is the directory the executor is confined to, on its host.
	RemoteRoot string `json:"remote_root"`
	// ExecProgram and ExecArgs are the transport, e.g. ssh and its arguments.
	ExecProgram string   `json:"exec_program"`
	ExecArgs    []string `json:"exec_args"`
	// Model is optional; empty leaves the harness default.
	Model string `json:"model,omitempty"`
	// APIBaseURL points the harness at a different Messages API endpoint.
	// Primarily for offline tests against a scripted model.
	APIBaseURL string `json:"api_base_url,omitempty"`
}

// Load reads and validates a config file.
func Load(path string) (Config, error) {
	var c Config
	body, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, errors.New("expected one config object")
	}
	return c, c.validate()
}

func (c Config) validate() error {
	for key, value := range map[string]string{
		"binary":       c.Binary,
		"runtime_home": c.RuntimeHome,
		"state_root":   c.StateRoot,
		"remote_root":  c.RemoteRoot,
		"exec_program": c.ExecProgram,
	} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s must be an absolute path", key)
		}
	}
	// A shared directory would let the harness's own writes land in the store
	// the trusted service claims sole ownership of.
	if filepath.Clean(c.RuntimeHome) == filepath.Clean(c.StateRoot) {
		return errors.New("runtime_home and state_root must differ")
	}
	return nil
}

// ValidateArgs restricts the command line to a single prompt.
//
// The rejected flags are the ones that could reintroduce a local execution
// domain or a second config source: --tools would re-add built-in tools,
// --mcp-config would add another server, --settings and --setting-sources
// would reopen configuration the trusted side is supposed to own, and
// --dangerously-skip-permissions speaks for itself.
func ValidateArgs(args []string) error {
	if len(args) != 1 {
		return errors.New("expected exactly one prompt argument")
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("unsupported argument %q; pass a prompt, not a flag", args[0])
	}
	return nil
}

// harnessArgs assembles the launch. Everything that seals the tool surface is
// here rather than in a settings file, because a flag cannot be overridden by
// a settings source the harness discovers later.
func harnessArgs(c Config, mcpConfig string, tools []string) []string {
	args := []string{
		"--print",
		// Remove every built-in tool. Measured on 2.1.263: this reduces the
		// advertised set to exactly the MCP tools, and an unregistered call is
		// refused by the harness rather than by the prompt. See
		// scripts/test-claude-toolface.py.
		"--tools", "",
		// Ignore user, project and local settings; refuse bypassPermissions.
		"--restricted",
		// Only the servers named below; none discovered from the environment.
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"--allowedTools", strings.Join(tools, ","),
		// Nobody is at a terminal to answer a prompt, and a prompt that cannot
		// be answered must deny rather than hang.
		"--permission-prompts", "none",
		// Skills, plugins and slash commands are separate state roots this
		// design does not mediate, so they are off rather than unmanaged.
		"--disable-slash-commands",
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return args
}

// harnessEnv builds the harness's environment explicitly. Nothing is inherited
// wholesale: an inherited variable is a channel the trusted side did not
// choose, and some of them (API keys, OAuth tokens) are exactly what must not
// leak into a subprocess environment later.
func harnessEnv(c Config, stateToken string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(c.RuntimeHome, "user-home"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(c.RuntimeHome, "config"),
		"RCA_STATE_TOKEN=" + stateToken,
		// Auto-memory writes to a state root this design does not mediate;
		// memory is served by the trusted state service instead.
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "TERM"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	if c.APIBaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+c.APIBaseURL)
	}
	return env
}

// mcpConfig renders the --mcp-config value: one HTTP server on loopback,
// authenticated by a per-run bearer token.
func mcpConfig(url, token string) (string, error) {
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"rca": map[string]any{
			"type":    "http",
			"url":     url,
			"headers": map[string]string{"Authorization": "Bearer " + token},
		},
	}})
	return string(body), err
}
