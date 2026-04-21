package version

// Version is the human-readable tag or branch the image was built from.
// Commit is the full git SHA of that build, used by the iOS client to
// compare against GitHub's HEAD to detect when a newer image is available.
// Both are stamped at build time via -ldflags "-X .../version.Version=...".
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)
