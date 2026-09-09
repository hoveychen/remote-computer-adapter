package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/hoveychen/remote-computer-adapter/internal/executor"
)

// DefaultReleaseBase is where published archives live.
const DefaultReleaseBase = "https://github.com/hoveychen/remote-computer-adapter/releases/latest/download"

// DefaultRemotePath is where the executor binary is installed.
const DefaultRemotePath = ".local/bin/rca"

// Options describes one deployment.
type Options struct {
	// Target is the ssh destination, e.g. "sandbox-host" or "user@1.2.3.4".
	Target string
	// RemotePath is where to install, relative to the remote home unless
	// absolute.
	RemotePath string
	// Binary is a local rca to push instead of downloading a release. It must
	// already match the remote platform; nothing here can verify that, so it is
	// the operator's assertion.
	Binary string
	// ReleaseBase overrides the download location.
	ReleaseBase string
	// VerifyRoot is a directory on the remote to prove the install works
	// against. Empty skips the handshake.
	VerifyRoot string
	// SSH and SCP are the transport commands, so a site with a wrapper can
	// name it.
	SSH, SCP string
	// Out receives progress.
	Out io.Writer
}

func (o *Options) defaults() {
	if o.RemotePath == "" {
		o.RemotePath = DefaultRemotePath
	}
	if o.ReleaseBase == "" {
		o.ReleaseBase = DefaultReleaseBase
	}
	if o.SSH == "" {
		o.SSH = "ssh"
	}
	if o.SCP == "" {
		o.SCP = "scp"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
}

// logf reports progress. It tolerates a nil Out rather than relying on
// defaults() having run first: a progress message is never worth a panic, and
// making the guarantee local means a new call site cannot get it wrong.
func (o *Options) logf(format string, args ...any) {
	if o.Out == nil {
		return
	}
	fmt.Fprintf(o.Out, format+"\n", args...)
}

// Run installs rca on the remote and reports the platform it installed for.
func Run(o Options) (Platform, error) {
	o.defaults()
	if strings.TrimSpace(o.Target) == "" {
		return Platform{}, errors.New("deploy requires an ssh target")
	}

	platform, err := detect(o)
	if err != nil {
		return platform, err
	}
	o.logf("remote platform: %s", platform)

	binary, cleanup, err := obtain(o, platform)
	if err != nil {
		return platform, err
	}
	defer cleanup()

	if err := push(o, binary); err != nil {
		return platform, err
	}
	o.logf("installed at %s", o.RemotePath)

	// Ask the installed binary what it is. A copy that landed but cannot run —
	// wrong architecture, missing loader, noexec mount — is the failure this
	// step exists to catch, and it is invisible from the sending side.
	version, err := run(o, shellQuote(o.RemotePath)+" version")
	if err != nil {
		return platform, fmt.Errorf("installed binary does not run: %w", err)
	}
	o.logf("remote reports: %s", strings.TrimSpace(version))

	if o.VerifyRoot != "" {
		if err := handshake(o); err != nil {
			return platform, err
		}
		o.logf("executor handshake ok on %s", o.VerifyRoot)
	}
	return platform, nil
}

func detect(o Options) (Platform, error) {
	out, err := run(o, "uname -s; uname -m")
	if err != nil {
		return Platform{}, fmt.Errorf("cannot reach %s: %w", o.Target, err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return Platform{}, fmt.Errorf("unexpected uname output %q", out)
	}
	return ParseUname(fields[0], fields[1])
}

// obtain returns a local path to the binary for platform, and a cleanup.
func obtain(o Options, platform Platform) (string, func(), error) {
	if o.Binary != "" {
		if _, err := os.Stat(o.Binary); err != nil {
			return "", func() {}, err
		}
		return o.Binary, func() {}, nil
	}
	url := o.ReleaseBase + "/" + platform.Archive()
	o.logf("downloading %s", url)
	sums, err := fetchChecksums(o.ReleaseBase)
	if err != nil {
		return "", func() {}, err
	}
	want, ok := sums[platform.Archive()]
	if !ok {
		return "", func() {}, fmt.Errorf("checksums.txt has no entry for %s", platform.Archive())
	}
	return download(url, want)
}

func fetchChecksums(base string) (map[string]string, error) {
	body, err := get(base + "/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("checksums: %w", err)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			sums[path.Base(fields[1])] = fields[0]
		}
	}
	if len(sums) == 0 {
		return nil, errors.New("checksums.txt is empty or unparsable")
	}
	return sums, nil
}

// download fetches the archive, checks it against the published digest, and
// extracts the rca entry. The digest is checked before anything is extracted:
// a mismatched archive is not unpacked at all.
func download(url, wantSum string) (string, func(), error) {
	body, err := get(url)
	if err != nil {
		return "", func() {}, err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != wantSum {
		return "", func() {}, fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, wantSum)
	}
	binary, err := extractRCA(body)
	if err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp("", "rca-deploy-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.Remove(file.Name()) }
	if _, err := file.Write(binary); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.Chmod(file.Name(), 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file.Name(), cleanup, nil
}

// extractRCA pulls the rca entry out of a release tarball.
func extractRCA(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("archive contains no rca entry")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || path.Base(header.Name) != "rca" {
			continue
		}
		// Bound the read: a hostile archive should not be able to claim an
		// arbitrary size and have it allocated here.
		const maxBinary = 256 << 20
		if header.Size > maxBinary {
			return nil, fmt.Errorf("rca entry is %d bytes, over the limit", header.Size)
		}
		return io.ReadAll(io.LimitReader(tr, maxBinary))
	}
}

func get(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// push copies the binary into place. It writes to a temporary name and moves
// it, so a running executor is never overwritten mid-read and a failed copy
// cannot leave a truncated binary at the install path.
func push(o Options, binary string) error {
	dir := path.Dir(o.RemotePath)
	if _, err := run(o, "mkdir -p "+shellQuote(dir)); err != nil {
		return fmt.Errorf("cannot create %s on the remote: %w", dir, err)
	}
	staged := o.RemotePath + ".incoming"
	// scp hands the destination to a shell on the far side, so it needs the
	// same quoting as any other remote path.
	cmd := exec.Command(o.SCP, "-q", binary, o.Target+":"+shellQuote(staged))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_, err := run(o, fmt.Sprintf("chmod 755 %s && mv -f %s %s",
		shellQuote(staged), shellQuote(staged), shellQuote(o.RemotePath)))
	return err
}

// handshake proves the installed binary serves the executor protocol, using
// the same client the harness uses rather than a bespoke check.
func handshake(o Options) error {
	client, err := executor.Dial(o.SSH,
		[]string{"-T", o.Target, shellQuote(o.RemotePath) + " serve --root " + shellQuote(o.VerifyRoot)},
		io.Discard)
	if err != nil {
		return fmt.Errorf("executor handshake: %w", err)
	}
	defer client.Close()
	var probe struct {
		Protocol int    `json:"protocol"`
		Root     string `json:"root"`
	}
	if err := client.Call(&executor.Request{Op: executor.OpVersion}, &probe); err != nil {
		return fmt.Errorf("executor handshake: %w", err)
	}
	// Report the resolved root: it is what remote_root must be set to, and
	// finding that out here beats finding it out when a session refuses to
	// start.
	o.logf("executor root resolves to %s", probe.Root)
	return nil
}

func run(o Options, command string) (string, error) {
	cmd := exec.Command(o.SSH, "-T", o.Target, command)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// shellQuote makes a value safe inside the single remote shell command ssh
// runs. Paths come from the operator, not the model, but a path with a space
// should still install rather than corrupt the command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
