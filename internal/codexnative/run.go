package codexnative

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
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoveychen/remote-computer-adapter/internal/runtimehome"
	"github.com/hoveychen/remote-computer-adapter/internal/trustedstate"
)

const marker = "RCA codex-native prototype v1\n"

func prepare(c Config) (*os.File, error) {
	return runtimehome.Prepare(c.RuntimeHome, marker, "user-home", "work")
}

func atomicWrite(dir, name, data string) error {
	return runtimehome.AtomicWrite(dir, name, data)
}
func harnessEnv(c Config, token string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(c.RuntimeHome, "user-home"), "CODEX_HOME=" + c.RuntimeHome, "RCA_STATE_TOKEN=" + token}
	// Credentials remain with the trusted harness. Never pass this list to the
	// transport or remote shell; their environments are independently constructed.
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "TERM"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}
func Run(c Config, args []string, rca string) error {
	if e := ValidateArgs(args); e != nil {
		return e
	}
	lock, e := prepare(c)
	if e != nil {
		return e
	}
	defer lock.Close()
	state, e := trustedstate.Open(c.StateRoot)
	if e != nil {
		return e
	}
	defer state.Close()
	tokenBytes := make([]byte, 32)
	if _, e = rand.Read(tokenBytes); e != nil {
		return e
	}
	token := hex.EncodeToString(tokenBytes)
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer listener.Close()
	nativeBytes := make([]byte, 32)
	if _, e = rand.Read(nativeBytes); e != nil {
		return e
	}
	nativeToken := hex.EncodeToString(nativeBytes)
	backgroundBytes := make([]byte, 32)
	if _, e = rand.Read(backgroundBytes); e != nil {
		return e
	}
	backgroundToken := hex.EncodeToString(backgroundBytes)
	installerBytes := make([]byte, 32)
	if _, e = rand.Read(installerBytes); e != nil {
		return e
	}
	installerToken := hex.EncodeToString(installerBytes)
	native, bindThread, storeID, e := nativeService(state, nativeToken, backgroundToken, installerToken)
	if e != nil {
		listener.Close()
		return e
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", trustedstate.HTTPHandler(state, token))
	mux.Handle("/native/v2/", native)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		server.Close()
	}()
	if e = atomicWrite(c.RuntimeHome, "environments.toml", environmentConfig(c, rca)); e != nil {
		return e
	}
	if e = atomicWrite(c.RuntimeHome, "config.toml", harnessConfig(c, "http://"+listener.Addr().String()+"/mcp")+nativeMemoryConfig("http://"+listener.Addr().String()+"/native/v2/", storeID)); e != nil {
		return e
	}
	// app-server accepts environment-native cwd separately from local config cwd.
	cmd := exec.Command(c.Binary, "app-server", "--strict-config")
	cmd.Dir = filepath.Join(c.RuntimeHome, "work")
	cmd.Env = append(harnessEnv(c, token), "RCA_NATIVE_MEMORY_TOKEN="+nativeToken, "RCA_NATIVE_MEMORY_BACKGROUND_TOKEN="+backgroundToken, "RCA_NATIVE_SKILLS_INSTALLER_TOKEN="+installerToken)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		stdin.Close()
		return e
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if e = cmd.Start(); e != nil {
		stdin.Close()
		return e
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- driveNativeSession(ctx, c, args, stdin, stdout, bindThread) }()
	select {
	case e := <-sessionDone:
		return e
	case sig := <-signals:
		if s, ok := sig.(syscall.Signal); ok {
			syscall.Kill(-cmd.Process.Pid, s)
		}
		return fmt.Errorf("interrupted: %s", sig)
	case e := <-serverErr:
		return fmt.Errorf("trusted state server stopped: %w", e)
	}
}
