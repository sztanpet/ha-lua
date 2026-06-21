# Repository-root Makefile. The Go project and its full build/test/lint
# targets live in the ha-lua/ add-on subfolder (see ha-lua/Makefile). This
# file covers the things that operate on the whole repository: pushing,
# cutting releases, and setting up a fresh development checkout.

.PHONY: install install-tools hooks check-browser push release

# One-shot setup for a fresh checkout: analyzers + git hooks. The browser
# probe runs last so its warning, if any, is the final thing on screen.
install: install-tools hooks check-browser

# The browser-driven UI tests (internal/lua, chromedp) need a Chrome/Chromium
# binary. They skip cleanly when none is found, so a missing browser is a
# warning, not an error. The probe order mirrors the tests' findChrome: the
# CHROMEDP_BROWSER override wins, then these names on $PATH, then the macOS
# app bundle. Set CHROMEDP_BROWSER=/path/to/chrome to point the tests at a
# specific binary.
CHROME_NAMES := google-chrome-stable google-chrome chromium-browser chromium headless-shell chrome
MACOS_CHROME := /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
check-browser:
	@if [ -n "$(CHROMEDP_BROWSER)" ]; then \
	    if [ -x "$(CHROMEDP_BROWSER)" ]; then \
	        echo "browser: using CHROMEDP_BROWSER=$(CHROMEDP_BROWSER)"; \
	    else \
	        echo "WARN: CHROMEDP_BROWSER=$(CHROMEDP_BROWSER) is not an executable file."; \
	    fi; \
	else \
	    found=""; \
	    for name in $(CHROME_NAMES); do \
	        if command -v $$name >/dev/null 2>&1; then found=$$name; break; fi; \
	    done; \
	    if [ -z "$$found" ] && [ -x "$(MACOS_CHROME)" ]; then found="$(MACOS_CHROME)"; fi; \
	    if [ -n "$$found" ]; then \
	        echo "browser: found $$found for the chromedp UI tests"; \
	    else \
	        echo "WARN: no Chrome/Chromium found; the chromedp browser UI tests will be skipped."; \
	        echo "      Install one (e.g. 'apt install chromium') or set CHROMEDP_BROWSER=/path/to/chrome."; \
	    fi; \
	fi

# Every analyzer (staticcheck, benchstat, golangci-lint) is a tool
# dependency — the `tool` directives in ha-lua/go.mod — so `go tool` builds
# each on demand at the version pinned there. Nothing is installed into a
# bin directory; this just pre-fetches the module sources so the first
# `go tool` run is offline-fast.
install-tools:
	cd ha-lua && go mod download

# Point git at the repo's pre-commit hook (gofmt + vet + staticcheck + lint).
hooks:
	git config core.hooksPath .githooks

# Delegate to the Go module's Makefile.
push:
	$(MAKE) -C ha-lua push

release:
	$(MAKE) -C ha-lua release VERSION=$(VERSION)
