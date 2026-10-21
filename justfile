set shell := ["bash", "-uc"]

# `npm ci --prefix frontend`, skipped when node_modules is already current:
# npm ci itself writes node_modules/.package-lock.json to match the lock it
# just installed, so comparing that marker's mtime against package-lock.json
# tells us whether the last install still matches. A prerequisite of test,
# build and dev; just only runs a given prerequisite once per invocation, so
# `just ci` (test then build) installs frontend deps at most once.
_frontend-deps:
    cd frontend && test node_modules/.package-lock.json -nt package-lock.json || npm ci

# The whole verification gate. Extended by later tasks; CI runs `just ci`.
test: _frontend-deps
    go vet ./...
    go test ./...
    cd frontend && npx vitest run
    cd frontend && npx tsc --noEmit

lint:
    shellcheck bin/statusline.sh bin/statusline-tap.sh \
        bin/usage-alert.sh bin/fetch-stores.sh bin/limits-poller.sh bin/check-upgrade.sh notifier/build.sh \
        packaging/release.sh packaging/update-tap.sh

generate: _frontend-deps
    buf lint
    buf generate

ci: lint test build dry-run-check

build: _frontend-deps
    buf generate
    NEXT_OUTPUT=export npm run build --prefix frontend
    mkdir -p internal/server/dist
    find internal/server/dist -mindepth 1 -depth ! -name README.md -delete
    cp -R frontend/out/. internal/server/dist/
    mkdir -p dist
    go build -trimpath \
        -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
        -o dist/claude-obs ./cmd/claude-obs

# Next dev server, proxying the Connect API (and any other server paths) to
# a local `claude-obs serve` (127.0.0.1:8427 by default) instead of the
# static export: see frontend/next.config.mjs's rewrites, which only apply
# when NEXT_OUTPUT is unset. Run `claude-obs serve` yourself in another
# terminal first (`just build && ./dist/claude-obs serve`, or via the
# installed launchd agent).
dev: _frontend-deps
    npm run dev --prefix frontend

install: build
    ./dist/claude-obs install

dry-run-check:
    ./dist/claude-obs install --dry-run --yes

# --upgrade retains the installed profile registry and recorded Claude binary;
# legacy PROFILE_DIR/PROFILE_DIRS migrate once with their history labels intact.
# Fresh installation detects only ~/.claude and ~/.codex. Additional profiles
# are entered manually in Settings, and an empty installation is valid.
# Initial history import runs in the background with status in Settings.
# --upgrade fails if there is no readable install at the target path.
#
# `just --list` shows only the LAST comment line, so the summary goes last:
# Rebuild and reinstall over an existing install, asking nothing.
_upgrade-check:
    bash bin/check-upgrade.sh

upgrade: _upgrade-check build
    ./dist/claude-obs install --upgrade

# The release workflow runs this for a pushed tag; see packaging/release.sh.
# Build the release tarballs for a version tag such as v1.2.3 into dist/release.
release version: build
    bash packaging/release.sh {{version}}
