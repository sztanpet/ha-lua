package web

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/sztanpet/ha-lua/internal/logbuf"
	"github.com/sztanpet/ha-lua/internal/lua"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// chromeNames mirrors the binaries chromedp's exec allocator searches for. We
// probe them up front so the browser tests skip cleanly on hosts and CI images
// without a browser installed, rather than failing make test/make check.
var chromeNames = []string{
	"google-chrome-stable", "google-chrome", "chromium-browser", "chromium",
	"headless-shell", "chrome",
}

func findChrome() string {
	if p := os.Getenv("CHROMEDP_BROWSER"); p != "" {
		return p
	}
	for _, name := range chromeNames {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "darwin" {
		const macChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(macChrome); err == nil {
			return macChrome
		}
	}
	return ""
}

func newBrowserCtx(t *testing.T) context.Context {
	t.Helper()
	browser := findChrome()
	if browser == "" {
		t.Skip("no Chrome/Chromium found (set CHROMEDP_BROWSER to override); skipping browser UI test")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browser),
		chromedp.NoSandbox,
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(cancelAlloc)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)
	boundedCtx, cancelTimeout := context.WithTimeout(browserCtx, 30*time.Second)
	t.Cleanup(cancelTimeout)
	return boundedCtx
}

func serveShell(t *testing.T, scripts map[string]string) *httptest.Server {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)

	// ha.log goes through the default logger, so the page only sees script
	// messages if the buffer is wired into it.
	logs := logbuf.New(64)
	previous := slog.Default()
	slog.SetDefault(slog.New(logbuf.NewHandler(slog.NewTextHandler(discard{}, nil), logs)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	dir := t.TempDir()
	reg := lua.NewRegistry()
	router := lua.NewRouter(reg)
	ctx, cancel := context.WithCancel(context.Background())

	for scriptID, src := range scripts {
		path := filepath.Join(dir, scriptID+".lua")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		r := lua.NewRunner(scriptID, dir, nil, nil, tracker, nil, store.New(writeDB, readDB, scriptID), global)
		reg.Add(r)
		done := make(chan struct{})
		go func() { defer close(done); r.Start(ctx, path) }()
		t.Cleanup(func() { <-done })

		select {
		case <-r.LoadedCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("script %s did not finish loading", scriptID)
		}
		router.Register(scriptID, r.Routes())
	}
	t.Cleanup(cancel)

	srv := httptest.NewServer(Handler(Deps{
		Scripts: func() []Script {
			all := reg.All()
			out := make([]Script, len(all))
			for i, r := range all {
				out[i] = r
			}
			return out
		},
		Router: router,
		Debug: DebugHandler(DebugDeps{
			Version: "test",
			Started: time.Now(),
			Runners: reg.All,
			Tracker: tracker,
			Logs:    logs,
		}),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func uiScript(title, body string) string {
	return `
ha.ui("` + title + `")
ha.serve("GET", "/", function(req)
  return 200, "<!DOCTYPE html><html><body><p id=who>` + body + `</p></body></html>",
    {["Content-Type"]="text/html"}
end)
`
}

func TestShellRendersTabsAndFramesPage(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"alpha": uiScript("Alpha", "alpha page"),
		"beta":  uiScript("Beta", "beta page"),
	})

	var tabTitles, tabHrefs []string
	var framed string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`nav a`, chromedp.ByQuery),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("nav a")).map(a => a.textContent)`, &tabTitles),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("nav a")).map(a => a.getAttribute("href"))`, &tabHrefs),
		// The framed document, not the shell's, proves composition.
		chromedp.Poll(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const who = doc && doc.getElementById("who");
			return who ? who.textContent : null;
		})()`, &framed, chromedp.WithPollingTimeout(10*time.Second)),
	); err != nil {
		t.Fatal(err)
	}

	if len(tabTitles) != 3 || tabTitles[0] != "Alpha" || tabTitles[1] != "Beta" || tabTitles[2] != "Debug" {
		t.Fatalf("tabs = %v, want [Alpha Beta Debug]", tabTitles)
	}
	for _, href := range tabHrefs {
		if href == "" || href[0] != '#' {
			t.Errorf("tab href = %q, want a hash link", href)
		}
	}
	if framed != "alpha page" {
		t.Fatalf("framed document = %q, want the alpha script's page", framed)
	}
}

func TestShellFrameGrowsToPageHeight(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"tall": tallScript("Tall", 400, ""),
	})

	var frameH, viewClientH, viewScrollH, innerScrollH float64
	var innerOverflow bool
	var framedOverflow string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`nav a`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("page").clientHeight > document.getElementById("view").clientHeight`,
			nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`(() => {
			const inner = document.getElementById("page").contentDocument.scrollingElement;
			return inner.scrollHeight > inner.clientHeight;
		})()`, &innerOverflow),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("page").contentDocument.documentElement).overflowY`, &framedOverflow),
		chromedp.Evaluate(`document.getElementById("page").clientHeight`, &frameH),
		chromedp.Evaluate(`document.getElementById("view").clientHeight`, &viewClientH),
		chromedp.Evaluate(`document.getElementById("view").scrollHeight`, &viewScrollH),
		chromedp.Evaluate(`document.getElementById("page").contentDocument.scrollingElement.scrollHeight`, &innerScrollH),
	); err != nil {
		t.Fatal(err)
	}

	if frameH < innerScrollH {
		t.Errorf("iframe height = %v, want at least the page's %v", frameH, innerScrollH)
	}
	if innerOverflow {
		t.Errorf("framed document can still scroll itself at frame height %v", frameH)
	}
	if framedOverflow != "hidden" {
		t.Errorf("framed document overflow-y = %q, want hidden so it cannot take a gesture", framedOverflow)
	}
	if viewScrollH <= viewClientH {
		t.Errorf("#view scrollHeight %v <= clientHeight %v: the shell has nothing to scroll",
			viewScrollH, viewClientH)
	}
}

func TestShellFrameGrowsForViewportSizedPage(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"full": tallScript("Full", 400, `html,body{height:100%;margin:0}`),
	})

	var innerScrollable bool
	var frameH, viewScrollH, viewClientH float64
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`nav a`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("page").clientHeight > document.getElementById("view").clientHeight`,
			nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`(() => {
			const inner = document.getElementById("page").contentDocument.scrollingElement;
			return inner.scrollHeight > inner.clientHeight + 1;
		})()`, &innerScrollable),
		chromedp.Evaluate(`document.getElementById("page").clientHeight`, &frameH),
		chromedp.Evaluate(`document.getElementById("view").scrollHeight`, &viewScrollH),
		chromedp.Evaluate(`document.getElementById("view").clientHeight`, &viewClientH),
	); err != nil {
		t.Fatal(err)
	}

	if innerScrollable {
		t.Errorf("framed document still scrolls itself at frame height %v", frameH)
	}
	if viewScrollH <= viewClientH {
		t.Errorf("#view scrollHeight %v <= clientHeight %v: the shell has nothing to scroll",
			viewScrollH, viewClientH)
	}
}

func tallScript(title string, rows int, css string) string {
	return `
ha.ui("` + title + `")
ha.serve("GET", "/", function(req)
  local parts = {"<!DOCTYPE html><html><head><style>` + css + `</style></head><body style='margin:0'>"}
  for i = 1, ` + strconv.Itoa(rows) + ` do
    parts[#parts+1] = "<p style='height:40.3px;margin:0'>row " .. i .. "</p>"
  end
  parts[#parts+1] = "</body></html>"
  return 200, table.concat(parts), {["Content-Type"]="text/html"}
end)
`
}

func TestShellSwitchesTabOnHash(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"alpha": uiScript("Alpha", "alpha page"),
		"beta":  uiScript("Beta", "beta page"),
	})

	var firstSrc, secondSrc, activeTitle string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`nav a.active`, chromedp.ByQuery),
		chromedp.AttributeValue(`#page`, "src", &firstSrc, nil, chromedp.ByQuery),
		chromedp.Click(`nav a:nth-child(2)`, chromedp.ByQuery),
		chromedp.Poll(`document.querySelector("#page").getAttribute("src") === "s/beta/"`, nil),
		chromedp.AttributeValue(`#page`, "src", &secondSrc, nil, chromedp.ByQuery),
		chromedp.Text(`nav a.active`, &activeTitle, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}

	if firstSrc != "s/alpha/" {
		t.Errorf("initial iframe src = %q, want s/alpha/", firstSrc)
	}
	if secondSrc != "s/beta/" {
		t.Errorf("iframe src after tab click = %q, want s/beta/", secondSrc)
	}
	if activeTitle != "Beta" {
		t.Errorf("active tab = %q, want Beta", activeTitle)
	}
}

func TestShellHonoursDeepLink(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"alpha": uiScript("Alpha", "alpha page"),
		"beta":  uiScript("Beta", "beta page"),
	})

	var src string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/#beta"),
		chromedp.WaitVisible(`nav a.active`, chromedp.ByQuery),
		chromedp.AttributeValue(`#page`, "src", &src, nil, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if src != "s/beta/" {
		t.Fatalf("iframe src = %q, want s/beta/", src)
	}
}

func TestDebugTabRendersInShell(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"alpha": uiScript("Alpha", "alpha page"),
		// Serves a page but never called ha.ui: unreachable from the tab bar,
		// which is the confusing case the debug page has to call out.
		"orphan": `
ha.serve("GET", "/", function(req) return 200, "<html></html>" end)
ha.serve("POST", "/api/thing", function(req) return 200, "ok" end)
`,
	})

	var lastTab, scriptCell, orphanTab, orphanRoutes, dumped string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/#debug"),
		chromedp.WaitVisible(`nav a.active`, chromedp.ByQuery),
		chromedp.Text(`nav a.active`, &lastTab, chromedp.ByQuery),
		chromedp.Poll(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const cell = doc && doc.querySelector("#scripts tbody td");
			return cell ? cell.textContent : null;
		})()`, &scriptCell, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const rows = Array.from(doc.querySelectorAll("#scripts tbody tr"));
			const row = rows.find(tr => tr.children[0].textContent === "orphan");
			return row ? row.children[1].textContent : "";
		})()`, &orphanTab),
		chromedp.Evaluate(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const rows = Array.from(doc.querySelectorAll("#scripts tbody tr"));
			const row = rows.find(tr => tr.children[0].textContent === "orphan");
			if (!row) return "";
			const cell = row.children[2];
			// Rendered line count, not the raw text: white-space must not eat
			// the newlines the cell is built with.
			const style = getComputedStyle(cell);
			return cell.textContent + "|" + style.whiteSpace;
		})()`, &orphanRoutes),
		chromedp.Evaluate(`document.getElementById("page").contentDocument.getElementById("dump").click()`, nil),
		chromedp.Poll(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const stacks = doc.getElementById("stacks");
			return stacks.hidden ? null : stacks.textContent;
		})()`, &dumped, chromedp.WithPollingTimeout(10*time.Second)),
	); err != nil {
		t.Fatal(err)
	}

	if lastTab != "Debug" {
		t.Errorf("active tab = %q, want Debug", lastTab)
	}
	if scriptCell != "alpha" {
		t.Errorf("scripts table lists %q, want alpha", scriptCell)
	}
	if !strings.Contains(orphanTab, "ha.ui") {
		t.Errorf("orphan script's tab cell = %q, want the ha.ui hint", orphanTab)
	}
	if !strings.Contains(orphanRoutes, "GET /\nPOST /api/thing|pre-line") {
		t.Errorf("routes cell = %q, want one route per rendered line", orphanRoutes)
	}
	if !strings.Contains(dumped, "goroutine ") {
		t.Errorf("stack dump = %q", dumped)
	}
}

func TestDebugLogPanelFiltersByScript(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveShell(t, map[string]string{
		"alpha": `ha.ui("Alpha")
ha.log("info", "alpha speaking")`,
		"beta": `ha.ui("Beta")
ha.log("warn", "beta speaking")`,
	})

	const lines = `(() => {
		const doc = document.getElementById("page").contentDocument;
		return Array.from(doc.querySelectorAll("#log div")).map(d => d.textContent).join("\n");
	})()`

	var sources []string
	var everything, filtered string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/#debug"),
		chromedp.WaitVisible(`nav a.active`, chromedp.ByQuery),
		chromedp.Poll(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const log = doc && doc.getElementById("log");
			const text = log ? log.textContent : "";
			return text.includes("alpha speaking") && text.includes("beta speaking") ? text : null;
		})()`, &everything, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`(() => {
			const doc = document.getElementById("page").contentDocument;
			return Array.from(doc.querySelectorAll("#source option")).map(o => o.value);
		})()`, &sources),
		chromedp.Evaluate(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const source = doc.getElementById("source");
			source.value = "alpha";
			source.dispatchEvent(new Event("change"));
		})()`, nil),
		chromedp.Poll(`(() => {
			const doc = document.getElementById("page").contentDocument;
			const text = doc.getElementById("log").textContent;
			return text.includes("alpha speaking") ? text : null;
		})()`, &filtered, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(lines, &filtered),
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(everything, "[alpha]") || !strings.Contains(everything, "[beta]") {
		t.Errorf("unfiltered log = %q, want both scripts tagged", everything)
	}
	want := []string{"", "*", "alpha", "beta"}
	if len(sources) != len(want) {
		t.Fatalf("source options = %v, want %v", sources, want)
	}
	for i, value := range want {
		if sources[i] != value {
			t.Fatalf("source options = %v, want %v", sources, want)
		}
	}
	// Every remaining line must be alpha's: the panel refetches from seq 0 on a
	// filter change, so beta's lines cannot survive on screen.
	for _, line := range strings.Split(filtered, "\n") {
		if line != "" && !strings.Contains(line, "[alpha]") {
			t.Errorf("filtered log kept %q; full text:\n%s", line, filtered)
		}
	}
}
