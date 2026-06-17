package lua

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// repoScriptsDir is the shipped example/script tree, relative to this package.
const repoScriptsDir = "../../scripts"

// TestShippedScriptsCompile loads every *.lua under scripts/ (compile only, no
// execution) so a syntax error in a shipped script is caught by `make test`
// rather than at runtime inside the daemon. ha.*/store.*/require references are
// fine here: LoadFile compiles but never runs the chunk.
func TestShippedScriptsCompile(t *testing.T) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	var found int
	err := filepath.Walk(repoScriptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".lua") {
			return nil
		}
		found++
		if _, lerr := L.LoadFile(path); lerr != nil {
			t.Errorf("%s: %v", path, lerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no scripts found to compile")
	}
}

// newScheduleState boots an LState whose require resolves into the repo's
// scripts/lib, so tests exercise the actual shipped lib/schedule.lua.
func newScheduleState(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	L.SetContext(context.Background())
	t.Cleanup(L.Close)
	RegisterStdlib(L, repoScriptsDir)
	return L
}

func TestSchedulePureLib(t *testing.T) {
	L := newScheduleState(t)

	// Drive the pure functions from Lua and let assert() surface failures as a
	// DoString error with a useful message.
	err := L.DoString(`
		local s = require "schedule"

		-- parse_hhmm
		assert(s.parse_hhmm("06:30") == 390, "parse 06:30")
		assert(s.parse_hhmm("00:00") == 0, "parse 00:00")
		assert(s.parse_hhmm("23:59") == 1439, "parse 23:59")
		assert(s.parse_hhmm("24:00") == nil, "reject 24:00")
		assert(s.parse_hhmm("6:30") == nil, "reject single-digit hour")
		assert(s.parse_hhmm("ab:cd") == nil, "reject non-numeric")

		local days = { ["0"] = {
			{time="06:30", temp=21}, {time="08:00", temp=18},
			{time="17:00", temp=21}, {time="22:00", temp=16},
		} }

		-- Mid-day: 09:00 -> the 08:00 step (idx 1), next is 17:00 (480 min away).
		local t, idx, nxt = s.resolve(days, 0, 9*60)
		assert(t == 18, "midday temp "..tostring(t))
		assert(idx == 1, "midday idx "..tostring(idx))
		assert(nxt == 480, "midday next "..tostring(nxt))

		-- Late night: 23:00 -> last step today (idx 3); next wraps to Tuesday.
		days["1"] = { {time="06:00", temp=20} }
		t, idx, nxt = s.resolve(days, 0, 23*60)
		assert(t == 16, "night temp "..tostring(t))
		assert(idx == 3, "night idx "..tostring(idx))
		assert(nxt == 1440 - 23*60 + 360, "night next "..tostring(nxt))

		-- Carryover before the first transition: Sunday's last step carries into
		-- early Monday, idx -1, next is Monday 06:30.
		local cd = {
			["6"] = { {time="22:00", temp=15} },
			["0"] = { {time="06:30", temp=21} },
		}
		t, idx, nxt = s.resolve(cd, 0, 5*60)
		assert(t == 15, "carry temp "..tostring(t))
		assert(idx == -1, "carry idx "..tostring(idx))
		assert(nxt == 90, "carry next "..tostring(nxt))

		-- Empty schedule: nil everywhere.
		t, idx, nxt = s.resolve({}, 0, 600)
		assert(t == nil and nxt == nil, "empty schedule")

		-- validate
		assert(s.validate(days) == true, "valid days")
		local ok, msg = s.validate({ ["0"] = { {time="99:99", temp=20} } })
		assert(ok == false and msg ~= nil, "bad time rejected")
		ok, msg = s.validate({ ["0"] = { {time="06:00", temp=99} } })
		assert(ok == false and msg ~= nil, "out-of-range temp rejected")
	`)
	if err != nil {
		t.Fatal(err)
	}
}
