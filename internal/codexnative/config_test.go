package codexnative

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyArgs(t *testing.T) {
	for _, a := range [][]string{{"exec", "--json", "--", "prompt"}, {"exec"}, {"exec", "--", "-c features.memories=true"}} {
		if e := ValidateArgs(a); e != nil {
			t.Fatal(e)
		}
	}
	for _, a := range [][]string{{"app-server"}, {"exec", "-c", "features.memories=true"}, {"exec", "--enable=memories"}, {"exec", "--ignore-user-config"}, {"exec", "--profile", "x"}, {"exec", "resume"}, {"exec", "--output-last-message", "/tmp/x"}, {"exec", "--", "x", "-c"}, {"exec", "--cd", "/tmp"}} {
		if e := ValidateArgs(a); e == nil {
			t.Fatal("accepted", a)
		}
	}
}
func TestManagedRuntime(t *testing.T) {
	c := Config{RuntimeHome: filepath.Join(t.TempDir(), "runtime")}
	lock, e := prepare(c)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := prepare(c); e == nil {
		other.Close()
		t.Fatal("parallel owner accepted")
	}
	lock.Close()
	lock, e = prepare(c)
	if e != nil {
		t.Fatal(e)
	}
	lock.Close()
	unmanaged := filepath.Join(t.TempDir(), "runtime")
	os.Mkdir(unmanaged, 0700)
	os.WriteFile(filepath.Join(unmanaged, "config.toml"), []byte("original"), 0600)
	if other, e := prepare(Config{RuntimeHome: unmanaged}); e == nil {
		other.Close()
		t.Fatal("adopted user home")
	}
	b, _ := os.ReadFile(filepath.Join(unmanaged, "config.toml"))
	if string(b) != "original" {
		t.Fatal("overwrote config")
	}
}
func TestGeneratedPolicy(t *testing.T) {
	c := Config{RuntimeHome: "/private/runtime", ExecProgram: "/usr/bin/ssh", ExecArgs: []string{"-a", "host", "codex exec-server --listen stdio"}}
	env := environmentConfig(c, "/trusted/rca")
	for _, want := range []string{"include_local = false", "default = \"third-party\"", "_native-transport"} {
		if !strings.Contains(env, want) {
			t.Fatal(env)
		}
	}
	config := harnessConfig(c, "http://127.0.0.1:1234/mcp")
	for _, want := range []string{"generate_memories = false", "use_memories = false", "memories = false", "inherit = \"none\"", "required = true", "bearer_token_env_var = \"RCA_STATE_TOKEN\""} {
		if !strings.Contains(config, want) {
			t.Fatal(config)
		}
	}
	t.Setenv("SECRET_CANARY", "never-copy")
	t.Setenv("DYLD_INSERT_LIBRARIES", "canary")
	t.Setenv("OPENAI_API_KEY", "trusted-key")
	child := strings.Join(harnessEnv(c, "token"), "\n")
	if strings.Contains(child, "never-copy") || strings.Contains(child, "canary") {
		t.Fatal("ambient env leaked")
	}
	if !strings.Contains(child, "OPENAI_API_KEY=trusted-key") {
		t.Fatal("trusted auth lost")
	}
}
