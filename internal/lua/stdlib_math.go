package lua

import (
	"math"

	lua "github.com/yuin/gopher-lua"
)

func registerMath(L *lua.LState) {
	mathTbl := L.GetGlobal("math").(*lua.LTable)

	mathTbl.RawSetString("round", L.NewFunction(luaMathRound))
	mathTbl.RawSetString("clamp", L.NewFunction(luaMathClamp))
	mathTbl.RawSetString("log2", L.NewFunction(luaMathLog2))
	mathTbl.RawSetString("sign", L.NewFunction(luaMathSign))
}

func luaMathRound(L *lua.LState) int {
	x := L.CheckNumber(1)
	L.Push(lua.LNumber(math.Round(float64(x))))
	return 1
}

func luaMathClamp(L *lua.LState) int {
	x := L.CheckNumber(1)
	min := L.CheckNumber(2)
	max := L.CheckNumber(3)
	if x < min {
		L.Push(lua.LNumber(min))
	} else if x > max {
		L.Push(lua.LNumber(max))
	} else {
		L.Push(lua.LNumber(x))
	}
	return 1
}

func luaMathLog2(L *lua.LState) int {
	x := L.CheckNumber(1)
	L.Push(lua.LNumber(math.Log2(float64(x))))
	return 1
}

func luaMathSign(L *lua.LState) int {
	x := L.CheckNumber(1)
	if x > 0 {
		L.Push(lua.LNumber(1))
	} else if x < 0 {
		L.Push(lua.LNumber(-1))
	} else {
		L.Push(lua.LNumber(0))
	}
	return 1
}
