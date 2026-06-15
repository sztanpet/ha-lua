package lua

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	lua "github.com/yuin/gopher-lua"
)

func registerCrypto(L *lua.LState) {
	mod := L.RegisterModule("crypto", cryptoFuncs)
	L.Push(mod)
}

var cryptoFuncs = map[string]lua.LGFunction{
	"md5":              luaCryptoMD5,
	"sha1":             luaCryptoSHA1,
	"sha256":           luaCryptoSHA256,
	"sha512":           luaCryptoSHA512,
	"hmac_sha256":      luaCryptoHMACSHA256,
	"hmac_sha512":      luaCryptoHMACSHA512,
	"base64_encode":    luaCryptoBase64Encode,
	"base64_decode":    luaCryptoBase64Decode,
	"base64url_encode": luaCryptoBase64URLEncode,
	"base64url_decode": luaCryptoBase64URLDecode,
	"hex_encode":       luaCryptoHexEncode,
	"hex_decode":       luaCryptoHexDecode,
	"random_bytes":     luaCryptoRandomBytes,
	"random_hex":       luaCryptoRandomHex,
	"equal":            luaCryptoEqual,
}

func luaCryptoMD5(L *lua.LState) int {
	s := L.CheckString(1)
	sum := md5.Sum([]byte(s))
	L.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func luaCryptoSHA1(L *lua.LState) int {
	s := L.CheckString(1)
	sum := sha1.Sum([]byte(s))
	L.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func luaCryptoSHA256(L *lua.LState) int {
	s := L.CheckString(1)
	sum := sha256.Sum256([]byte(s))
	L.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func luaCryptoSHA512(L *lua.LState) int {
	s := L.CheckString(1)
	sum := sha512.Sum512([]byte(s))
	L.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func luaCryptoHMACSHA256(L *lua.LState) int {
	key := L.CheckString(1)
	msg := L.CheckString(2)
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	L.Push(lua.LString(hex.EncodeToString(h.Sum(nil))))
	return 1
}

func luaCryptoHMACSHA512(L *lua.LState) int {
	key := L.CheckString(1)
	msg := L.CheckString(2)
	h := hmac.New(sha512.New, []byte(key))
	h.Write([]byte(msg))
	L.Push(lua.LString(hex.EncodeToString(h.Sum(nil))))
	return 1
}

func luaCryptoBase64Encode(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(s))))
	return 1
}

func luaCryptoBase64Decode(L *lua.LState) int {
	s := L.CheckString(1)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	L.Push(lua.LString(b))
	return 1
}

func luaCryptoBase64URLEncode(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(base64.RawURLEncoding.EncodeToString([]byte(s))))
	return 1
}

func luaCryptoBase64URLDecode(L *lua.LState) int {
	s := L.CheckString(1)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	L.Push(lua.LString(b))
	return 1
}

func luaCryptoHexEncode(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(hex.EncodeToString([]byte(s))))
	return 1
}

func luaCryptoHexDecode(L *lua.LState) int {
	s := L.CheckString(1)
	b, err := hex.DecodeString(s)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	L.Push(lua.LString(b))
	return 1
}

func luaCryptoRandomBytes(L *lua.LState) int {
	n := L.CheckInt(1)
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		L.RaiseError("crypto.random_bytes: %v", err)
		return 0
	}
	L.Push(lua.LString(b))
	return 1
}

func luaCryptoRandomHex(L *lua.LState) int {
	n := L.CheckInt(1)
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		L.RaiseError("crypto.random_hex: %v", err)
		return 0
	}
	L.Push(lua.LString(hex.EncodeToString(b)))
	return 1
}

func luaCryptoEqual(L *lua.LState) int {
	a := L.CheckString(1)
	b := L.CheckString(2)
	L.Push(lua.LBool(subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1))
	return 1
}
