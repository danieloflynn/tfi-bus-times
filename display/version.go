package display

// Version is the build version string shown in the top-left corner of the
// board, opposite the timestamp, so it's easy to confirm which release is live.
//
// It is injected at build time by the linker, e.g.
//
//	go build -ldflags "-X tfi-display/display.Version=v43" .
//
// The Makefile and .github/workflows/release.yml set it to "v" + the git commit
// count, so it auto-increments on every release without any manual bump.
// Un-injected builds (a bare `go build`, or a checkout with no git history)
// fall back to "dev", which usefully flags a local/dev binary on the board.
var Version = "dev"
