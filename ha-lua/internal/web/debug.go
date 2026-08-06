package web

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/go-json-experiment/json"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/logbuf"
	"github.com/sztanpet/ha-lua/internal/lua"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
)

// DebugDeps are the introspection sources behind /debug/. Every field is
// optional: a nil source leaves its section out rather than failing the page.
type DebugDeps struct {
	Version   string
	Started   time.Time
	PprofAddr string
	DBPath    string

	Runners   func() []*lua.Runner
	Scheduler *scheduler.Scheduler
	Tracker   *state.Tracker
	Client    *ha.Client
	Logs      *logbuf.Buffer

	RetentionDays int
	PurgeInterval time.Duration
}

type runtimeInfo struct {
	Version    string    `json:"version"`
	Started    time.Time `json:"started,omitzero"`
	Uptime     string    `json:"uptime"`
	Go         string    `json:"go"`
	GOMAXPROCS int       `json:"gomaxprocs"`
	Goroutines int       `json:"goroutines"`
	HeapAlloc  uint64    `json:"heap_alloc"`
	HeapSys    uint64    `json:"heap_sys"`
	NumGC      uint32    `json:"num_gc"`
	PprofAddr  string    `json:"pprof_addr,omitempty"`
}

type storageInfo struct {
	Path          string `json:"path,omitempty"`
	Size          int64  `json:"size"`
	Entities      int    `json:"entities"`
	WriteQueueLen int    `json:"write_queue_len"`
	WriteQueueCap int    `json:"write_queue_cap"`
	RetentionDays int    `json:"retention_days"`
	PurgeInterval string `json:"purge_interval"`
}

type scriptInfo struct {
	lua.RunnerStats
	Timers []scheduler.TimerInfo `json:"timers"`
}

type debugInfo struct {
	Runtime runtimeInfo  `json:"runtime"`
	HA      *ha.Stats    `json:"ha,omitempty"`
	Scripts []scriptInfo `json:"scripts"`
	Storage storageInfo  `json:"storage"`
}

type logsReply struct {
	Records []logbuf.Record `json:"records"`
	Newest  uint64          `json:"newest"`
}

// DebugHandler serves the debug page and its polling APIs under /debug/.
func DebugHandler(deps DebugDeps) http.Handler {
	page, err := assets.ReadFile("assets/debug.html")
	if err != nil {
		panic("web: embedded debug.html missing: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, collectInfo(deps))
	})
	mux.HandleFunc("/debug/api/logs", func(w http.ResponseWriter, r *http.Request) {
		serveLogs(w, r, deps)
	})
	mux.HandleFunc("/debug/api/goroutines", serveGoroutines)
	mux.HandleFunc("/debug/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})
	return mux
}

func collectInfo(deps DebugDeps) debugInfo {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	info := debugInfo{
		Runtime: runtimeInfo{
			Version:    deps.Version,
			Started:    deps.Started,
			Go:         runtime.Version(),
			GOMAXPROCS: runtime.GOMAXPROCS(0),
			Goroutines: runtime.NumGoroutine(),
			HeapAlloc:  mem.HeapAlloc,
			HeapSys:    mem.HeapSys,
			NumGC:      mem.NumGC,
			PprofAddr:  deps.PprofAddr,
		},
		Scripts: []scriptInfo{},
		Storage: storageInfo{
			Path:          deps.DBPath,
			RetentionDays: deps.RetentionDays,
		},
	}
	if !deps.Started.IsZero() {
		info.Runtime.Uptime = time.Since(deps.Started).Round(time.Second).String()
	}
	if deps.PurgeInterval > 0 {
		info.Storage.PurgeInterval = deps.PurgeInterval.String()
	}
	if deps.Client != nil {
		st := deps.Client.Stats()
		info.HA = &st
	}
	if deps.Tracker != nil {
		st := deps.Tracker.Stats()
		info.Storage.Entities = st.Entities
		info.Storage.WriteQueueLen = st.WriteQueueLen
		info.Storage.WriteQueueCap = st.WriteQueueCap
	}
	if deps.DBPath != "" {
		if fi, err := os.Stat(deps.DBPath); err == nil {
			info.Storage.Size = fi.Size()
		}
	}

	byScript := map[string][]scheduler.TimerInfo{}
	if deps.Scheduler != nil {
		for _, timer := range deps.Scheduler.Timers() {
			byScript[timer.ScriptID] = append(byScript[timer.ScriptID], timer)
		}
	}
	if deps.Runners != nil {
		for _, runner := range deps.Runners() {
			stats := runner.Stats()
			timers := byScript[stats.ScriptID]
			if timers == nil {
				timers = []scheduler.TimerInfo{}
			}
			info.Scripts = append(info.Scripts, scriptInfo{RunnerStats: stats, Timers: timers})
		}
	}
	return info
}

func serveLogs(w http.ResponseWriter, r *http.Request, deps DebugDeps) {
	if deps.Logs == nil {
		writeJSON(w, logsReply{Records: []logbuf.Record{}})
		return
	}

	query := logbuf.Query{Level: slog.LevelDebug, Script: r.URL.Query().Get("script")}
	if raw := r.URL.Query().Get("since"); raw != "" {
		query.Since, _ = strconv.ParseUint(raw, 10, 64)
	}
	if raw := r.URL.Query().Get("level"); raw != "" {
		if err := query.Level.UnmarshalText([]byte(raw)); err != nil {
			http.Error(w, "bad level", http.StatusBadRequest)
			return
		}
	}

	records, newest := deps.Logs.Snapshot(query)
	writeJSON(w, logsReply{Records: records, Newest: newest})
}

// serveGoroutines dumps every goroutine's stack. On demand only — it stops the
// world briefly — and independent of debug.pprof_addr, which is usually off.
func serveGoroutines(w http.ResponseWriter, r *http.Request) {
	profile := pprof.Lookup("goroutine")
	if profile == nil {
		http.Error(w, "goroutine profile unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := profile.WriteTo(w, 2); err != nil {
		slog.Warn("web: goroutine dump failed", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.MarshalWrite(w, v, json.Deterministic(true))
}
