package claudenative

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hoveychen/remote-computer-adapter/internal/executor"
	"github.com/hoveychen/remote-computer-adapter/internal/runtimehome"
	"github.com/hoveychen/remote-computer-adapter/internal/toolgateway"
	"github.com/hoveychen/remote-computer-adapter/internal/trustedstate"
)

const marker = "RCA claude-native v1\n"

func token() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// Run launches the harness and returns when the session ends.
//
// The order matters: the runtime home is claimed and locked, the state service
// is listening, and the config naming it is written, all before the harness
// starts. A harness that came up first could observe a directory without the
// config and fall back to defaults.
func Run(c Config, args []string) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := ValidateArgs(args); err != nil {
		return err
	}
	lock, err := runtimehome.Prepare(c.RuntimeHome, marker, "user-home", "config")
	if err != nil {
		return err
	}
	defer lock.Close()

	state, err := trustedstate.Open(c.StateRoot)
	if err != nil {
		return err
	}
	defer state.Close()

	stateToken, err := token()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()

	// Dial before the harness starts. A transport that only fails once the
	// model has already asked for a file would look to it like an empty
	// workspace rather than a broken session.
	remote, err := executor.Dial(c.ExecProgram, c.ExecArgs, os.Stderr)
	if err != nil {
		return fmt.Errorf("remote executor: %w", err)
	}
	defer remote.Close()
	var probe struct {
		Protocol int    `json:"protocol"`
		Root     string `json:"root"`
	}
	if err := remote.Call(&executor.Request{Op: executor.OpVersion}, &probe); err != nil {
		return fmt.Errorf("remote executor handshake: %w", err)
	}
	if probe.Root != c.RemoteRoot {
		return fmt.Errorf("remote executor is serving %q, not the configured %q", probe.Root, c.RemoteRoot)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", trustedstate.HTTPHandler(toolgateway.New(state, remote), stateToken))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		server.Close()
	}()

	config, err := mcpConfig("http://"+listener.Addr().String()+"/mcp", stateToken)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.Binary, harnessArgs(c, config, toolgateway.ToolNames())...)
	cmd.Dir = c.RuntimeHome
	cmd.Env = harnessEnv(c, stateToken)
	// The prompt goes on stdin: --tools is variadic, so a trailing positional
	// would be collected as another tool name and silently re-open the surface.
	cmd.Stdin = strings.NewReader(args[0])
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case sig := <-signals:
		if s, ok := sig.(syscall.Signal); ok {
			syscall.Kill(-cmd.Process.Pid, s)
		}
		<-done
		return fmt.Errorf("interrupted: %s", sig)
	case err := <-serverErr:
		// Without the state service the harness has no memory or skills and no
		// remote backend. Ending the session is the honest outcome; letting it
		// continue would leave the model with a silently emptier world.
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			<-done
		}
		return fmt.Errorf("trusted state server stopped: %w", err)
	}
}
