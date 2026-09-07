// Package codexnative launches a trusted Codex harness with a remote-only
// native execution registry. It never uses RCA's legacy syscall interception.
package codexnative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Binary      string   `json:"binary"`
	RuntimeHome string   `json:"runtime_home"`
	StateRoot   string   `json:"state_root"`
	RemoteCWD   string   `json:"remote_cwd"`
	ExecProgram string   `json:"exec_program"`
	ExecArgs    []string `json:"exec_args"`
	Model       string   `json:"model,omitempty"`
	// Optional trusted provider endpoint, primarily for offline mock-model tests.
	ProviderURL string `json:"provider_url,omitempty"`
}

func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, errors.New("expected one config object")
	}
	for key, v := range map[string]string{"binary": c.Binary, "runtime_home": c.RuntimeHome, "state_root": c.StateRoot, "remote_cwd": c.RemoteCWD, "exec_program": c.ExecProgram} {
		if !filepath.IsAbs(v) || strings.ContainsAny(v, "\x00\r\n") {
			return c, fmt.Errorf("%s must be an absolute path", key)
		}
	}
	if filepath.Clean(c.RuntimeHome) == filepath.Clean(c.StateRoot) {
		return c, errors.New("runtime and state directories must differ")
	}
	return c, nil
}
func quote(s string) string { return strconv.Quote(s) }
func array(a []string) string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = quote(v)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// ValidateArgs intentionally supports only fresh noninteractive exec runs.
// A strict allowlist prevents profiles, config overrides, helper subcommands,
// resume metadata and local file-output flags from changing the prototype policy.
func ValidateArgs(args []string) error {
	if len(args) == 0 || args[0] != "exec" {
		return errors.New("prototype supports only: exec [--json] [--color auto|always|never] -- <prompt>; config/profile/resume overrides are not supported")
	}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--":
			if len(args)-i > 2 {
				return errors.New("expected at most one prompt after --")
			}
			return nil
		case "--json":
		case "--color":
			i++
			if i >= len(args) || (args[i] != "auto" && args[i] != "always" && args[i] != "never") {
				return errors.New("invalid --color")
			}
		default:
			return fmt.Errorf("unsupported Codex argument %q; put prompt after --", args[i])
		}
	}
	return nil
}

func environmentConfig(c Config, rca string) string {
	// The helper clears inherited env before invoking the trusted transport argv.
	return "default = \"third-party\"\ninclude_local = false\n\n[[environments]]\nid = \"third-party\"\nprogram = " + quote(rca) + "\nargs = " + array(append([]string{"_native-transport", "--", c.ExecProgram}, c.ExecArgs...)) + "\ncwd = " + quote(c.RuntimeHome) + "\n"
}
func harnessConfig(c Config, url string) string {
	config := `approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
allow_login_shell = false
check_for_update_on_startup = false
cli_auth_credentials_store = "file"

[features]
memories = true
hooks = false
apps = false
remote_plugin = false
multi_agent = false
shell_snapshot = false
skill_mcp_dependency_install = false

[memories]
generate_memories = false
use_memories = true
dedicated_tools = true

[shell_environment_policy]
inherit = "none"
ignore_default_excludes = false

[mcp_servers.rca_state]
required = true
url = ` + quote(url) + `
bearer_token_env_var = "RCA_STATE_TOKEN"

`
	// Use the installed version's default local HTTP placement. No local
	// environment is registered and no experimental placement key is assumed.
	if c.Model != "" {
		config = "model = " + quote(c.Model) + "\n" + config
	}
	if c.ProviderURL != "" {
		config = "model_provider = \"rca_prototype\"\n" + config + "\n[model_providers.rca_prototype]\nname = \"RCA prototype provider\"\nbase_url = " + quote(c.ProviderURL) + "\nwire_api = \"responses\"\nrequires_openai_auth = false\n"
	}
	return config
}
