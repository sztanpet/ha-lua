package lua

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestLuaJSONRoundTrip(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	cases := []struct {
		name string
		lua  string // Lua expression that produces the value
		want string // expected JSON
	}{
		{"nil", "nil", "null"},
		{"bool_true", "true", "true"},
		{"bool_false", "false", "false"},
		{"int", "42", "42"},
		{"float", "3.14", "3.14"},
		{"string", `"hello"`, `"hello"`},
		{"array", "{1,2,3}", "[1,2,3]"},
		{"object", `{a=1,b="x"}`, `{"a":1,"b":"x"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := L.DoString("_v = " + tc.lua); err != nil {
				t.Fatalf("lua: %v", err)
			}
			v := L.GetGlobal("_v")
			out, err := luaMarshal(L, v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(out) != tc.want {
				t.Errorf("want %s, got %s", tc.want, out)
			}
		})
	}
}

func TestJSONToLua(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	cases := []struct {
		name  string
		json  string
		check func(*testing.T, lua.LValue)
	}{
		{"null", "null", func(t *testing.T, v lua.LValue) {
			if v != lua.LNil {
				t.Errorf("want nil, got %v", v)
			}
		}},
		{"bool", "true", func(t *testing.T, v lua.LValue) {
			if v != lua.LTrue {
				t.Errorf("want true, got %v", v)
			}
		}},
		{"number", "42", func(t *testing.T, v lua.LValue) {
			if n, ok := v.(lua.LNumber); !ok || n != 42 {
				t.Errorf("want 42, got %v", v)
			}
		}},
		{"string", `"hi"`, func(t *testing.T, v lua.LValue) {
			if s, ok := v.(lua.LString); !ok || string(s) != "hi" {
				t.Errorf("want 'hi', got %v", v)
			}
		}},
		{"array", `[1,2,3]`, func(t *testing.T, v lua.LValue) {
			tbl, ok := v.(*lua.LTable)
			if !ok {
				t.Fatal("want table")
			}
			if tbl.RawGetInt(1) != lua.LNumber(1) {
				t.Errorf("arr[1]: want 1, got %v", tbl.RawGetInt(1))
			}
		}},
		{"object", `{"x":99}`, func(t *testing.T, v lua.LValue) {
			tbl, ok := v.(*lua.LTable)
			if !ok {
				t.Fatal("want table")
			}
			if tbl.RawGetString("x") != lua.LNumber(99) {
				t.Errorf("obj.x: want 99, got %v", tbl.RawGetString("x"))
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := luaUnmarshal(L, []byte(tc.json))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tc.check(t, v)
		})
	}
}

// TestLuaMarshalCyclicTable: a table that reaches itself must come back as an
// error. Before the depth cap this recursed until the goroutine stack
// overflowed, which is a Go *fatal* error — unrecoverable by pcall, so a single
// script's bad payload killed the daemon and every other script with it.
func TestLuaMarshalCyclicTable(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	cases := map[string]string{
		"direct":   `_v = {}; _v.self = _v`,
		"indirect": `_v = {}; local mid = { back = _v }; _v.mid = mid`,
		"array":    `_v = {}; _v[1] = _v`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if err := L.DoString(src); err != nil {
				t.Fatalf("lua: %v", err)
			}
			if _, err := luaMarshal(L, L.GetGlobal("_v")); err == nil {
				t.Fatal("marshal of a cyclic table succeeded, want an error")
			}
		})
	}
}

// TestJSONEncodeCyclicIsCatchable: the Lua-visible half of the same guard —
// json.encode raises an error the script can pcall, and the VM survives.
func TestJSONEncodeCyclicIsCatchable(t *testing.T) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	RegisterStdlib(L, t.TempDir(), nil)

	if err := L.DoString(`
		local t = {}
		t.self = t
		local ok, err = pcall(function() return json.encode(t) end)
		assert(not ok, "json.encode of a cyclic table must fail")
		assert(type(err) == "string" and err:find("nested"), "unexpected error: " .. tostring(err))
		-- the VM is still usable afterwards
		assert(json.encode({a = 1}) == '{"a":1}')
	`); err != nil {
		t.Fatal(err)
	}
}

// TestLuaMarshalDeepButFinite: the cap must not reject payloads that merely
// nest a few levels, which real HA attribute tables do.
func TestLuaMarshalDeepButFinite(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`
		_v = {}
		local cur = _v
		for i = 1, 20 do cur.next = {}; cur = cur.next end
	`); err != nil {
		t.Fatalf("lua: %v", err)
	}
	if _, err := luaMarshal(L, L.GetGlobal("_v")); err != nil {
		t.Fatalf("20 levels of nesting rejected: %v", err)
	}
}

func BenchmarkLuaJSONEncode(b *testing.B) {
	L := lua.NewState()
	defer L.Close()
	_ = L.DoString(`_attrs = {
		brightness=200, color_temp=4000, friendly_name="Bedroom Light",
		supported_features=63, color_mode="color_temp", min_mireds=153,
		max_mireds=500, effect_list={"colorloop","random"},
		hs_color={30,80}, rgb_color={255,165,0},
		xy_color={0.528,0.413}, is_on=true, effect=nil
	}`)
	v := L.GetGlobal("_attrs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = luaMarshal(L, v)
	}
}

func BenchmarkLuaJSONDecode(b *testing.B) {
	payload := []byte(`{"brightness":200,"color_temp":4000,"friendly_name":"Bedroom Light","supported_features":63,"color_mode":"color_temp","min_mireds":153,"max_mireds":500,"effect_list":["colorloop","random"],"hs_color":[30,80],"rgb_color":[255,165,0],"xy_color":[0.528,0.413],"is_on":true}`)
	L := lua.NewState()
	defer L.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = luaUnmarshal(L, payload)
	}
}
