package lua

import (
	"fmt"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/logwriter"
)

// smtpSendMail is swapped out in tests.
var smtpSendMail = smtp.SendMail

// maxExceptionLogBytes caps each ha.exceptions.log_file path (active + one
// rotated backup) so a script that keeps throwing can't fill /config.
const maxExceptionLogBytes = 5 << 20 // 5 MiB

// registerExceptionHandlers installs ha.exceptions.email and ha.exceptions.log_file.
func registerExceptionHandlers(L *lua.LState, t *lua.LTable) {
	L.SetField(t, "email", L.NewFunction(func(L *lua.LState) int {
		cfg := L.CheckTable(1)
		getString := func(key string) string {
			v := cfg.RawGetString(key)
			if s, ok := v.(lua.LString); ok {
				return string(s)
			}
			return ""
		}
		getInt := func(key string, def int) int {
			v := cfg.RawGetString(key)
			if n, ok := v.(lua.LNumber); ok {
				return int(n)
			}
			return def
		}

		toField := getString("to")
		smtpHost := getString("smtp_host")
		smtpPort := getInt("smtp_port", 587)
		username := getString("username")
		password := getString("password")
		from := getString("from")
		if from == "" {
			from = username
		}
		subjectPrefix := getString("subject_prefix")
		if subjectPrefix == "" {
			subjectPrefix = "[ha-lua]"
		}
		cooldown := 15 * time.Minute
		if s := getString("cooldown"); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				L.RaiseError("ha.exceptions.email: bad cooldown %q: %v", s, err)
				return 0
			}
			cooldown = d
		}

		// Cooldown state. The handler runs only on its script's goroutine,
		// so no locking is needed. lastAttempt counts failed sends too —
		// a broken SMTP config must not be retried on every event either.
		var lastAttempt time.Time
		var suppressed int
		var suppressedSince string

		handler := L.NewFunction(func(L *lua.LState) int {
			info := L.CheckTable(1)
			scriptID := luaStrField(info, "script_id")
			errMsg := luaStrField(info, "error")
			traceback := luaStrField(info, "traceback")
			callback := luaStrField(info, "callback")
			timestamp := luaStrField(info, "timestamp")

			if !lastAttempt.IsZero() && time.Since(lastAttempt) < cooldown {
				if suppressed == 0 {
					suppressedSince = timestamp
				}
				suppressed++
				return 0
			}

			var eventJSON string
			if ev := info.RawGetString("event"); ev != lua.LNil {
				if b, err := luaMarshal(L, ev); err == nil {
					eventJSON = string(b)
				}
			}

			subject := fmt.Sprintf("%s Error in script: %s", subjectPrefix, scriptID)
			body := buildEmailBody(scriptID, timestamp, callback, errMsg, traceback, eventJSON)
			if suppressed > 0 {
				body += fmt.Sprintf("\n%d similar errors suppressed since %s\n",
					suppressed, suppressedSince)
			}
			addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
			msg := buildSMTPMessage(from, toField, subject, body)
			auth := smtp.PlainAuth("", username, password, smtpHost)
			lastAttempt = time.Now()
			suppressed = 0
			if err := smtpSendMail(addr, auth, from, []string{toField}, []byte(msg)); err != nil {
				L.RaiseError("ha.exceptions.email: %v", err)
			}
			return 0
		})
		L.Push(handler)
		return 1
	}))

	L.SetField(t, "log_file", L.NewFunction(func(L *lua.LState) int {
		path := L.CheckString(1)
		handler := L.NewFunction(func(L *lua.LState) int {
			info := L.CheckTable(1)
			scriptID := luaStrField(info, "script_id")
			errMsg := luaStrField(info, "error")
			traceback := luaStrField(info, "traceback")
			callback := luaStrField(info, "callback")
			timestamp := luaStrField(info, "timestamp")

			var eventJSON string
			if ev := info.RawGetString("event"); ev != lua.LNil {
				if b, err := luaMarshal(L, ev); err == nil {
					eventJSON = string(b)
				}
			}

			body := buildEmailBody(scriptID, timestamp, callback, errMsg, traceback, eventJSON)
			// Create the parent dir so paths under e.g. /config/ha-lua/logs
			// work on a fresh install before anything else has written there.
			if dir := filepath.Dir(path); dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					L.RaiseError("ha.exceptions.log_file: %v", err)
					return 0
				}
			}
			// Cap the file before appending, so it can't grow without bound.
			logwriter.RotateIfLarge(path, maxExceptionLogBytes)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				L.RaiseError("ha.exceptions.log_file: %v", err)
				return 0
			}
			entry := fmt.Sprintf("---\n%s\n", body)
			_, werr := f.WriteString(entry)
			// Close before raising: RaiseError unwinds via panic and a
			// deferred Close would swallow its own error anyway.
			cerr := f.Close()
			if werr != nil {
				L.RaiseError("ha.exceptions.log_file write: %v", werr)
				return 0
			}
			if cerr != nil {
				L.RaiseError("ha.exceptions.log_file close: %v", cerr)
			}
			return 0
		})
		L.Push(handler)
		return 1
	}))
}

func luaStrField(t *lua.LTable, key string) string {
	v := t.RawGetString(key)
	if s, ok := v.(lua.LString); ok {
		return string(s)
	}
	return ""
}

func buildEmailBody(scriptID, timestamp, callback, errMsg, traceback, eventJSON string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Script:   %s\n", scriptID)
	fmt.Fprintf(&b, "Time:     %s\n", timestamp)
	fmt.Fprintf(&b, "Callback: %s\n\n", callback)
	fmt.Fprintf(&b, "Error:\n  %s\n\n", errMsg)
	if traceback != "" {
		fmt.Fprintf(&b, "Traceback:\n  %s\n\n", strings.ReplaceAll(traceback, "\n", "\n  "))
	}
	if eventJSON != "" {
		fmt.Fprintf(&b, "Triggering event:\n  %s\n", eventJSON)
	}
	return b.String()
}

func buildSMTPMessage(from, to, subject, body string) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		from, to, subject, time.Now().UTC().Format(time.RFC1123Z), body)
}
