package web

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

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

	if len(tabTitles) != 2 || tabTitles[0] != "Alpha" || tabTitles[1] != "Beta" {
		t.Fatalf("tabs = %v, want [Alpha Beta]", tabTitles)
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
