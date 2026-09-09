package claudenative

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// unmediatedRoots are directories a Claude Code version could start writing
// state into that this design does not mediate. Memory and skills are served
// by the trusted state service; plugins, agents and hooks carry their own
// capability grants that a package import must not be able to widen.
//
// None of them appear today. They are listed because the harness is a closed
// binary: its internal write paths cannot be enumerated, so the only honest
// posture is to notice new ones rather than to assert none exist.
var unmediatedRoots = []string{
	"memory",
	"memories",
	"skills",
	"plugins",
	"agents",
	"hooks",
	"commands",
	"output-styles",
}

// AuditRuntime reports state roots that appeared inside the runtime home and
// bypass the trusted store.
//
// It runs after the session, so it cannot prevent the write — that is the
// point. A closed harness that starts persisting somewhere new does so
// silently, and a detector that reports it beats an assumption that says it
// cannot happen.
func AuditRuntime(runtimeHome string) []string {
	found := []string{}
	for _, base := range []string{
		filepath.Join(runtimeHome, "config"),
		filepath.Join(runtimeHome, "user-home", ".claude"),
	} {
		for _, name := range unmediatedRoots {
			path := filepath.Join(base, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.IsDir() {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) == 0 {
					continue // created but unused; not yet state
				}
			}
			found = append(found, path)
		}
	}
	sort.Strings(found)
	return found
}

// reportAudit writes the finding to stderr. It is a warning rather than a
// failure: the session has already ended, and turning a completed run into an
// error would hide its result behind a problem the operator can only act on
// next time.
func reportAudit(runtimeHome string, out *os.File) {
	found := AuditRuntime(runtimeHome)
	if len(found) == 0 {
		return
	}
	fmt.Fprintf(out, "rca: warning: the harness wrote state this design does not mediate:\n")
	for _, path := range found {
		fmt.Fprintf(out, "  %s\n", path)
	}
	fmt.Fprintf(out, "rca: that state is outside the trusted store's audit; treat this session's memory/skills claims as incomplete\n")
}
