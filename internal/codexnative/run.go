package codexnative

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoveychen/remote-adapter/internal/trustedstate"
	"golang.org/x/sys/unix"
)

const marker = "RCA codex-native prototype v1\n"

func prepare(c Config) (*os.File, error) {
	if e := os.MkdirAll(c.RuntimeHome, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(c.RuntimeHome)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runtime_home must be a private directory (0700)")
	}
	entries, e := os.ReadDir(c.RuntimeHome)
	if e != nil {
		return nil, e
	}
	b, e := os.ReadFile(filepath.Join(c.RuntimeHome, ".rca-native"))
	if len(entries) > 0 && (e != nil || string(b) != marker) {
		return nil, errors.New("refusing unmanaged nonempty runtime_home; choose a new directory")
	}
	fd, e := unix.Open(filepath.Join(c.RuntimeHome, ".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	lock := os.NewFile(uintptr(fd), "native runtime lock")
	if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("runtime_home is already in use")
	}
	if e = atomicWrite(c.RuntimeHome, ".rca-native", marker); e != nil {
		lock.Close()
		return nil, e
	}
	for _, dir := range []string{"user-home", "work"} {
		path := filepath.Join(c.RuntimeHome, dir)
		if e = os.MkdirAll(path, 0700); e != nil {
			lock.Close()
			return nil, e
		}
		info, e := os.Lstat(path)
		if e != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			lock.Close()
			return nil, fmt.Errorf("runtime %s must be a private directory", dir)
		}
	}
	return lock, nil
}
func atomicWrite(dir, name, data string) error {
	f, e := os.CreateTemp(dir, ".config-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.WriteString(data); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
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
	server := &http.Server{Handler: trustedstate.HTTPHandler(state, token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
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
	if e = atomicWrite(c.RuntimeHome, "config.toml", harnessConfig(c, "http://"+listener.Addr().String()+"/mcp")); e != nil {
		return e
	}
	// The only model-facing cwd is the fixed executor cwd. The OS process starts
	// in a private trusted directory so ambient project config isn't imported.
	launchArgs := append([]string{"exec", "--strict-config", "--skip-git-repo-check", "--cd", c.RemoteCWD}, args[1:]...)
	cmd := exec.Command(c.Binary, launchArgs...)
	cmd.Dir = filepath.Join(c.RuntimeHome, "work")
	cmd.Env = harnessEnv(c, token)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case e := <-done:
			return e
		case sig := <-signals:
			if s, ok := sig.(syscall.Signal); ok {
				syscall.Kill(-cmd.Process.Pid, s)
			}
		case e := <-serverErr:
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			<-done
			return fmt.Errorf("trusted state server stopped: %w", e)
		}
	}
}
