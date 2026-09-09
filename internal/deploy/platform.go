// Package deploy installs and upgrades rca on a remote executor host.
//
// The remote side of this product is one binary and one command. What makes
// deployment worth automating is not the copy — it is everything around it:
// picking the build that matches the remote's platform rather than the
// operator's, proving the bytes arrived intact, and proving the installed
// binary actually answers before anyone depends on it.
package deploy

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform is a Go build target.
type Platform struct {
	OS   string
	Arch string
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Archive is the release asset name for this platform. Release names carry no
// version so install one-liners can use releases/latest/download/.
func (p Platform) Archive() string { return "rca_" + p.OS + "_" + p.Arch + ".tar.gz" }

// Local is the platform this binary was built for.
func Local() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// supported mirrors what scripts/build-release.sh publishes. A platform that
// is not built is an error here rather than a download that 404s later.
var supported = []Platform{
	{"darwin", "arm64"}, {"darwin", "amd64"},
	{"linux", "amd64"}, {"linux", "arm64"},
}

// ParseUname maps `uname -s` and `uname -m` onto a released platform.
//
// The mapping is explicit rather than a lowercase-and-hope: an unrecognised
// pair must fail here, where the operator can read the values that confused
// it, instead of silently installing a binary the remote cannot execute.
func ParseUname(unameS, unameM string) (Platform, error) {
	var p Platform
	switch strings.ToLower(strings.TrimSpace(unameS)) {
	case "darwin":
		p.OS = "darwin"
	case "linux":
		p.OS = "linux"
	default:
		return p, fmt.Errorf("unsupported remote OS %q (uname -s)", strings.TrimSpace(unameS))
	}
	switch strings.ToLower(strings.TrimSpace(unameM)) {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "arm64", "aarch64":
		p.Arch = "arm64"
	default:
		return p, fmt.Errorf("unsupported remote architecture %q (uname -m)", strings.TrimSpace(unameM))
	}
	for _, s := range supported {
		if s == p {
			return p, nil
		}
	}
	return p, fmt.Errorf("no released build for %s", p)
}
