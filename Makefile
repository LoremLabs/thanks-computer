.PHONY: all lint test build clean install ui smoke version bump-version

include .env
export

GIT_HASH = $(shell git rev-parse HEAD)
BUILD_TS = $(shell date +%s)

all: lint test build
benchmark:
	cd ./chassis && make benchmark
clean:
	cd ./chassis && make clean
fetch-tools:
	cd ./chassis && make fetch-tools
lint:
	cd ./chassis && make lint
test:
	cd ./chassis && make test
qtest:
	cd ./chassis && make qtest
cover:
	cd ./chassis && make cover

# Build the admin-ui Svelte bundle into chassis/server/admin/ui/dist/
# so the next `go build` embeds the current sources. Degrades gracefully
# when admin-ui/ or pnpm is missing (CI without Node, fresh template
# clones) — the chassis still compiles, /admin/ just serves the
# "bundle not built" placeholder. Set SKIP_UI=1 to force-skip even when
# pnpm is available (useful for fast Go-only iteration).
ui:
	@if [ -n "$$SKIP_UI" ]; then \
		echo "==> skipping admin UI build (SKIP_UI set)"; \
	elif [ ! -d admin-ui ]; then \
		echo "==> admin-ui/ missing; skipping UI build"; \
	elif ! command -v pnpm >/dev/null 2>&1; then \
		echo "==> pnpm not on PATH; skipping UI build (install pnpm to embed an up-to-date bundle)"; \
	else \
		if [ ! -d admin-ui/node_modules ]; then \
			echo "==> pnpm install (admin-ui, first run)..."; \
			(cd admin-ui && pnpm install) || exit $$?; \
		fi; \
		echo "==> pnpm run build (admin-ui)..."; \
		(cd admin-ui && pnpm run build) || exit $$?; \
	fi
	@if [ -n "$$SKIP_UI" ]; then \
		echo "==> skipping continuation UI build (SKIP_UI set)"; \
	elif [ ! -d continuation-ui ]; then \
		echo "==> continuation-ui/ missing; skipping UI build"; \
	elif ! command -v pnpm >/dev/null 2>&1; then \
		echo "==> pnpm not on PATH; skipping continuation UI build (install pnpm to embed an up-to-date bundle)"; \
	else \
		if [ ! -d continuation-ui/node_modules ]; then \
			echo "==> pnpm install (continuation-ui, first run)..."; \
			(cd continuation-ui && pnpm install) || exit $$?; \
		fi; \
		echo "==> pnpm run build (continuation-ui)..."; \
		(cd continuation-ui && pnpm run build) || exit $$?; \
	fi

# Build the txco binary locally (chassis/bin/txco) after refreshing the
# embedded UI bundle. `make build SKIP_UI=1` bypasses the UI step.
build: ui
	cd ./chassis && make build

# Installs `txco` to $GOBIN (default ~/go/bin). Make sure that directory
# is on your PATH:
#
#   echo 'export PATH="$$(go env GOPATH)/bin:$$PATH"' >> ~/.zshrc
#
# Then `txco serve / init / apply / dev / diff` work from anywhere.
# Rebuilds admin-ui first so /admin/ matches your local source.
install: ui
	cd ./chassis && make install
dev:
	nodemon --watch './**/*.go' --signal SIGTERM --exec 'go' run ./cmd/txco

# End-to-end operator smokes. Each script cold-boots a chassis in a
# temp workspace, exercises a load-bearing surface (auth, secrets,
# fleet sync, …), and asserts the contract. They're scoped — failing
# a smoke is loud and specific. Useful as a pre-commit confidence
# check and as a CI gate against operator-visible regressions.
#
# Smokes build txco themselves (or honor a pre-built TXCO=… path),
# so `make smoke` runs from a clean tree without a prior `make build`.
# Add new smokes here as scripts/<name>-smoke.sh.
smoke:
	@set -e; \
	failed=0; \
	for s in scripts/secrets-smoke.sh scripts/control-sync-smoke.sh scripts/examples-smoke.sh; do \
		echo; echo "════════ $$s ════════"; \
		if ! bash "$$s"; then failed=$$((failed+1)); fi; \
	done; \
	if [ "$$failed" -gt 0 ]; then \
		echo; echo "❌ $$failed smoke(s) failed"; exit 1; \
	else \
		echo; echo "✅ all smokes passed"; \
	fi

# Print the current source-tree version (the dev fallback baked into the
# binary when ldflags aren't set). Released binaries get their version
# from the git tag — see .github/workflows/release.yml.
version:
	@awk -F'"' '/^VERSION/ {print $$2; exit}' chassis/Makefile

# Bump the in-source version in both spots that hardcode it. Default is a
# patch bump (0.2.0 -> 0.2.1); pass V= to set an exact version:
#
#   make bump-version              # patch bump
#   make bump-version V=0.3.0      # explicit (use for minor/major/rc)
#
# An rc tag (e.g. v0.3.0-rc1) publishes a GitHub prerelease and does not
# move the Homebrew tap formula.
bump-version:
	@cur=$$(make -s version); \
		if [ -n "$(V)" ]; then \
			new="$(V)"; \
		else \
			base=$$(echo "$$cur" | sed -E 's/-.*$$//'); \
			major=$$(echo "$$base" | cut -d. -f1); \
			minor=$$(echo "$$base" | cut -d. -f2); \
			patch=$$(echo "$$base" | cut -d. -f3); \
			new="$$major.$$minor.$$((patch+1))"; \
		fi; \
		echo "$$new" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { \
			echo "bump-version: '$$new' doesn't look like semver (x.y.z or x.y.z-rcN)"; exit 2; \
		}; \
		if [ "$$cur" = "$$new" ]; then \
			echo "bump-version: already at $$cur"; exit 0; \
		fi; \
		sed -i.bak -E "s/^VERSION = \"[^\"]*\"/VERSION = \"$$new\"/" chassis/Makefile && rm -f chassis/Makefile.bak; \
		sed -i.bak -E "s/^(	Version[[:space:]]*= )\"[^\"]*\"/\1\"$$new\"/" cmd/txco/main.go && rm -f cmd/txco/main.go.bak; \
		echo "bumped $$cur -> $$new"; \
		echo; \
		echo "to cut the release, run:"; \
		echo; \
		echo "  git commit -am 'release: v$$new'"; \
		echo "  git tag v$$new"; \
		echo "  git push origin main v$$new"; \
		echo; \
		echo "(an rc tag like v$$new-rc1 publishes a GitHub prerelease and skips the Homebrew tap update)"
