// Package version carries the build identity of the agentshare binary.
package version

// These are overridden at build time with
//
//	-ldflags "-X github.com/adfoke/agentshare/internal/version.Version=... "
var (
	Version = "0.1.0"
	Commit  = "dev"
	Date    = "unknown"
)

// Full returns a single human-readable version line.
func Full() string {
	if Commit == "" || Commit == "dev" {
		return "agentshare " + Version + " (uncommitted build)"
	}
	return "agentshare " + Version + " (" + Commit + " " + Date + ")"
}

// APIVersion is the version of the HTTP surface itself, independent of the
// binary's build number. It appears in /  and /openapi.json.
const APIVersion = "v1"
