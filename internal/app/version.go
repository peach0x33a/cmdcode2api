package app

// Version is the build's release tag. It defaults to "dev" for a plain
// `go build` and is overridden at release time via
// `-ldflags "-X cmdcode2api/internal/app.Version=<tag>"` (see
// .github/workflows/release.yml and scripts/deploy.sh). The web UI footer
// and GET /health both surface it.
var Version = "dev"
