package lua

import (
	"time"

	lua "github.com/yuin/gopher-lua"
)

const timeUserdataName = "time"

func registerTime(L *lua.LState) {
	mt := L.NewTypeMetatable(timeUserdataName)
	L.SetGlobal("time", L.NewTable())
	mod := L.GetGlobal("time").(*lua.LTable)

	mod.RawSetString("now", L.NewFunction(luaTimeNow))
	mod.RawSetString("parse", L.NewFunction(luaTimeParse))
	mod.RawSetString("unix", L.NewFunction(luaTimeUnix))
	mod.RawSetString("parse_duration", L.NewFunction(luaTimeParseDuration))

	mod.RawSetString("RFC3339", lua.LString(time.RFC3339))
	mod.RawSetString("second", lua.LNumber(1))
	mod.RawSetString("minute", lua.LNumber(60))
	mod.RawSetString("hour", lua.LNumber(3600))
	mod.RawSetString("day", lua.LNumber(86400))

	mt.RawSetString("__index", L.SetFuncs(L.NewTable(), timeMethods))
}

var timeMethods = map[string]lua.LGFunction{
	"format":     luaTimeFormat,
	"unix":       luaTimeGetUnix,
	"add":        luaTimeAdd,
	"sub":        luaTimeSub,
	"before":     luaTimeBefore,
	"after":      luaTimeAfter,
	"year":       luaTimeYear,
	"month":      luaTimeMonth,
	"day":        luaTimeDay,
	"hour":       luaTimeHour,
	"minute":     luaTimeMinute,
	"second":     luaTimeSecond,
	"weekday":    luaTimeWeekday,
	"is_zero":    luaTimeIsZero,
	"__tostring": luaTimeToString,
}

func pushTime(L *lua.LState, t time.Time) {
	ud := L.NewUserData()
	ud.Value = &t
	L.SetMetatable(ud, L.GetTypeMetatable(timeUserdataName))
	L.Push(ud)
}

func luaTimeNow(L *lua.LState) int {
	pushTime(L, time.Now())
	return 1
}

func luaTimeParse(L *lua.LState) int {
	layout := L.CheckString(1)
	value := L.CheckString(2)
	t, err := time.Parse(layout, value)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	pushTime(L, t)
	return 1
}

func luaTimeUnix(L *lua.LState) int {
	sec := L.CheckNumber(1)
	pushTime(L, time.Unix(int64(sec), 0))
	return 1
}

func luaTimeParseDuration(L *lua.LState) int {
	s := L.CheckString(1)
	d, err := time.ParseDuration(s)
	if err != nil {
		L.RaiseError("time.parse_duration: %v", err)
		return 0
	}
	L.Push(lua.LNumber(d.Seconds()))
	return 1
}

func getTime(L *lua.LState, n int) time.Time {
	ud := L.CheckUserData(n)
	if t, ok := ud.Value.(*time.Time); ok {
		return *t
	}
	L.ArgError(n, "time expected")
	return time.Time{}
}

func luaTimeFormat(L *lua.LState) int {
	t := getTime(L, 1)
	layout := L.CheckString(2)
	L.Push(lua.LString(t.Format(layout)))
	return 1
}

func luaTimeGetUnix(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Unix()))
	return 1
}

func luaTimeAdd(L *lua.LState) int {
	t := getTime(L, 1)
	sec := L.CheckNumber(2)
	pushTime(L, t.Add(time.Duration(float64(sec)*float64(time.Second))))
	return 1
}

func luaTimeSub(L *lua.LState) int {
	t := getTime(L, 1)
	other := getTime(L, 2)
	L.Push(lua.LNumber(t.Sub(other).Seconds()))
	return 1
}

func luaTimeBefore(L *lua.LState) int {
	t := getTime(L, 1)
	other := getTime(L, 2)
	L.Push(lua.LBool(t.Before(other)))
	return 1
}

func luaTimeAfter(L *lua.LState) int {
	t := getTime(L, 1)
	other := getTime(L, 2)
	L.Push(lua.LBool(t.After(other)))
	return 1
}

func luaTimeYear(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Year()))
	return 1
}

func luaTimeMonth(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(int(t.Month())))
	return 1
}

func luaTimeDay(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Day()))
	return 1
}

func luaTimeHour(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Hour()))
	return 1
}

func luaTimeMinute(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Minute()))
	return 1
}

func luaTimeSecond(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(t.Second()))
	return 1
}

func luaTimeWeekday(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LNumber(int(t.Weekday())))
	return 1
}

func luaTimeIsZero(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LBool(t.IsZero()))
	return 1
}

func luaTimeToString(L *lua.LState) int {
	t := getTime(L, 1)
	L.Push(lua.LString(t.Format(time.RFC3339)))
	return 1
}
