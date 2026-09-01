Payload directory for rnv3-setup.

`go run ./tools/release` (deploy/release.ps1 or release.sh) copies the Pi-side
files here before building the tool:
  rnv3, rnv3-migrate (linux/arm64), install.sh, cutover.sh, rnv3.service,
  config.example.yaml.
They are git-ignored; a plain `go build ./cmd/rnv3-setup` produces a tool
that needs --payload-dir.
