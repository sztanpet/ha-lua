package web

import (
	"embed"
	"io/fs"
	"net/http"
	"sort"

	"github.com/go-json-experiment/json"

	"github.com/sztanpet/ha-lua/internal/lua"
)

//go:embed assets
var assets embed.FS

// Script is what the tab bar needs from a running script. An interface so the
// shell can be tested without standing up Lua VMs.
type Script interface {
	ScriptID() string
	UITitle() string
}

// Deps are the subsystems the web shell reads. It mirrors lua.Deps: one struct
// so adding a source later does not churn every call site.
type Deps struct {
	// Called per request: scripts come and go with hot reload.
	Scripts func() []Script
	Router  *lua.Router
	// Nil leaves the Debug tab out of the tab bar entirely.
	Debug http.Handler
}

// Path is relative to the shell; anything absolute breaks under HA ingress.
type tab struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

const debugPrefix = "/debug/"

// Handler builds the daemon's HTTP surface: the shell at /, its assets, the tab
// list, the debug page, and every script's routes under /s/.
func Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()

	if deps.Router != nil {
		mux.Handle(lua.Mount, deps.Router)
	}
	if deps.Debug != nil {
		mux.Handle(debugPrefix, deps.Debug)
	}
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("web: embedded assets missing: " + err.Error())
	}
	mux.Handle("/ui/", http.StripPrefix("/ui", http.FileServerFS(sub)))
	mux.HandleFunc("/api/tabs", func(w http.ResponseWriter, r *http.Request) {
		serveTabs(w, r, deps)
	})

	shell, err := assets.ReadFile("assets/shell.html")
	if err != nil {
		panic("web: embedded shell.html missing: " + err.Error())
	}
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(shell)
	})

	return mux
}

func serveTabs(w http.ResponseWriter, r *http.Request, deps Deps) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	out := []tab{}
	if deps.Scripts != nil {
		for _, script := range deps.Scripts() {
			title := script.UITitle()
			if title == "" {
				continue // no ha.ui
			}
			id := script.ScriptID()
			out = append(out, tab{ID: id, Title: title, Path: lua.Mount[1:] + id + "/"})
		}
	}
	// Stable on top of Scripts()'s id order, so equal titles cannot swap either.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	if deps.Debug != nil {
		out = append(out, tab{ID: "debug", Title: "Debug", Path: debugPrefix[1:]})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.MarshalWrite(w, out, json.Deterministic(true))
}
