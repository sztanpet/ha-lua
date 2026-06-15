package lua

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// newStdlibState builds an LState with full RegisterStdlib and a temp scripts dir.
func newStdlibState(t testing.TB) (*lua.LState, string) {
	if t != nil {
		t.Helper()
	}
	dir := os.TempDir()
	if t != nil {
		dir = t.TempDir()
	}
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	L.SetContext(context.Background())
	if t != nil {
		t.Cleanup(L.Close)
	}
	RegisterStdlib(L, dir)
	return L, dir
}

func TestSandboxing(t *testing.T) {
	L, _ := newStdlibState(t)

	// Verify dangerous globals are nil
	for _, name := range []string{"load", "loadstring", "loadfile", "dofile", "module", "package", "io", "debug"} {
		if L.GetGlobal(name) != lua.LNil {
			t.Errorf("global %q should be nil", name)
		}
	}

	// Verify restricted os module
	err := L.DoString(`
		assert(os.clock ~= nil)
		assert(os.date ~= nil)
		assert(os.difftime ~= nil)
		assert(os.time ~= nil)
		assert(os.execute == nil)
		assert(os.exit == nil)
		assert(os.remove == nil)
		assert(os.rename == nil)
		assert(os.getenv == nil)
	`)
	if err != nil {
		t.Error(err)
	}
}

func newRequireState(t *testing.T, libs map[string]string) *lua.LState {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range libs {
		if err := os.WriteFile(filepath.Join(libDir, name+".lua"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	t.Cleanup(L.Close)
	RegisterStdlib(L, dir)
	return L
}

func TestRequireCachesModules(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"counter": `executed = (executed or 0) + 1; return { n = executed }`,
	})
	err := L.DoString(`
		local a = require("counter")
		local b = require("counter")
		assert(rawequal(a, b), "second require must return the cached table")
		assert(executed == 1, "module body ran " .. tostring(executed) .. " times")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequireCircularErrors(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"a": `return require("b")`,
		"b": `return require("a")`,
	})
	err := L.DoString(`require("a")`)
	if err == nil || !strings.Contains(err.Error(), "circular require") {
		t.Fatalf("want circular require error, got %v", err)
	}
}

func TestRequireNoReturnYieldsTrue(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"sideeffect": `did = true`,
	})
	err := L.DoString(`
		local v = require("sideeffect")
		assert(v == true, "module without a return value must yield true")
		assert(did == true, "module body must have run")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequireOutsideLibFails(t *testing.T) {
	for _, path := range []string{"../secret", "/etc/passwd", "lib/../../x"} {
		L := newRequireState(t, nil)
		err := L.DoString(`require("` + path + `")`)
		if err == nil || !strings.Contains(err.Error(), "outside scripts/lib") {
			t.Errorf("require(%q): want path error, got %v", path, err)
		}
	}
}

func TestMathAugmentation(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		assert(math.round(1.4) == 1)
		assert(math.round(1.5) == 2)
		assert(math.round(-1.5) == -2)
		assert(math.clamp(5, 0, 10) == 5)
		assert(math.clamp(-5, 0, 10) == 0)
		assert(math.clamp(15, 0, 10) == 10)
		assert(math.log2(8) == 3)
		assert(math.sign(5) == 1)
		assert(math.sign(-5) == -1)
		assert(math.sign(0) == 0)
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestStringsModule(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		assert(strings.contains("hello world", "world"))
		assert(strings.has_prefix("hello world", "hello"))
		assert(strings.has_suffix("hello world", "world"))
		
		local parts = strings.split("a,b,c", ",")
		assert(#parts == 3)
		assert(parts[1] == "a")
		
		assert(strings.join({"a", "b", "c"}, "-") == "a-b-c")
		assert(strings.trim_space("  hello  ") == "hello")
		assert(strings.trim("!!hello!!", "!") == "hello")
		assert(strings.replace_all("banana", "a", "o") == "bonono")
		assert(strings.count("banana", "a") == 3)
		
		local f = strings.fields("  a b  c ")
		assert(#f == 3)
		assert(f[3] == "c")
		
		assert(strings.to_upper("hello") == "HELLO")
		assert(strings.to_lower("HELLO") == "hello")
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestTimeModule(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		local now = time.now()
		assert(now:unix() > 0)
		
		local t = time.parse(time.RFC3339, "2026-06-15T12:00:00Z")
		assert(t:year() == 2026)
		assert(t:month() == 6)
		assert(t:day() == 15)
		assert(t:hour() == 12)
		assert(t:format(time.RFC3339) == "2026-06-15T12:00:00Z")
		
		local t2 = t:add(10)
		assert(t2:sub(t) == 10)
		assert(t:before(t2))
		assert(t2:after(t))
		
		assert(time.parse_duration("1h5m") == 3900)
		assert(time.minute == 60)
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestJSONModule(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		local tbl = { a = 1, b = { c = true } }
		local s = json.encode(tbl)
		-- json/v2 deterministic order: {"a":1,"b":{"c":true}}
		assert(s == '{"a":1,"b":{"c":true}}')
		
		local d = json.decode(s)
		assert(d.a == 1)
		assert(d.b.c == true)
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestREModule(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		assert(re.match("^hello", "hello world"))
		assert(not re.match("^world", "hello world"))
		
		assert(re.find("o..o", "hello world") == "o wo")
		
		local all = re.find_all("a.", "banana")
		assert(#all == 2)
		assert(all[1] == "an")
		
		assert(re.replace("a", "banana", "o") == "bonono")
		
		local parts = re.split(",", "a,b,c")
		assert(#parts == 3)
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestCryptoModule(t *testing.T) {
	L, _ := newStdlibState(t)
	err := L.DoString(`
		assert(crypto.md5("hello") == "5d41402abc4b2a76b9719d911017c592")
		assert(crypto.sha256("hello") == "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
		
		local key = "secret"
		local msg = "hello"
		local h = crypto.hmac_sha256(key, msg)
		assert(h == "88aab3ede8d3adf94d26ab90d3bafd4a2083070c3bcce9c014ee04a443847c0b")
		
		assert(crypto.base64_encode("hello") == "aGVsbG8=")
		assert(crypto.base64_decode("aGVsbG8=") == "hello")
		
		assert(crypto.hex_encode("hello") == "68656c6c6f")
		assert(crypto.hex_decode("68656c6c6f") == "hello")
		
		local b = crypto.random_bytes(16)
		assert(#b == 16)
		
		assert(crypto.equal("foo", "foo"))
		assert(not crypto.equal("foo", "bar"))
	`)
	if err != nil {
		t.Error(err)
	}
}

func TestHTTPModule(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", r.Header.Get("X-Test"))
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	L, _ := newStdlibState(t)
	L.SetGlobal("server_url", lua.LString(ts.URL))
	err := L.DoString(`
		local res, err = http.get(server_url, {["X-Test"] = "foo"})
		if not res then error(err) end
		assert(res.status == 200)
		assert(res.body == "ok")
		assert(res.headers["X-Test"] == "foo")
		
		res, err = http.post(server_url, "data", "text/plain", {["X-Test"] = "bar"})
		if not res then error(err) end
		assert(res.status == 200)
		assert(res.body == "ok")
		assert(res.headers["X-Test"] == "bar")
	`)
	if err != nil {
		t.Error(err)
	}
}

func BenchmarkReMatchCached(b *testing.B) {
	L, _ := newStdlibState(b)
	_ = L.DoString(`pattern = [[^sensor\.(temperature|humidity)_]]`)
	_ = L.DoString(`s = "sensor.temperature_living_room"`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = L.DoString(`re.match(pattern, s)`)
	}
}

func BenchmarkReMatchCold(b *testing.B) {
	L, _ := newStdlibState(b)
	_ = L.DoString(`s = "sensor.temperature_living_room"`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = L.DoString(fmt.Sprintf(`re.match("pattern%d", s)`, i))
	}
}

func BenchmarkHTTPGet(b *testing.B) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	L, _ := newStdlibState(b)
	L.SetGlobal("server_url", lua.LString(ts.URL))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = L.DoString(`http.get(server_url)`)
	}
}

func BenchmarkTimeNow(b *testing.B) {
	L, _ := newStdlibState(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = L.DoString(`time.now()`)
	}
}

func BenchmarkCryptoSHA256(b *testing.B) {
	L, _ := newStdlibState(b)
	payload := strings.Repeat("a", 1024)
	L.SetGlobal("payload", lua.LString(payload))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = L.DoString(`crypto.sha256(payload)`)
	}
}
