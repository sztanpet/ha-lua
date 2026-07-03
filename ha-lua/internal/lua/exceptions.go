package lua

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/logwriter"
)

// smtpSendMail is swapped out in tests.
var smtpSendMail = sendMailTimeout

// smtpTimeout bounds the entire SMTP exchange, dial included. The handler runs
// on the script goroutine and smtp.SendMail has no deadline anywhere — a
// wedged SMTP server would block the goroutine in Go code, which the
// supervisor's VM abort cannot interrupt, so StopScript would hang forever.
const smtpTimeout = 30 * time.Second

// sendMailTimeout is smtp.SendMail (STARTTLS when offered, then auth) on a
// connection with an absolute deadline covering the whole exchange.
func sendMailTimeout(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	return sendMailDeadline(addr, auth, from, to, msg, smtpTimeout)
}

// sendMailDeadline is sendMailTimeout with the deadline injectable by tests.
func sendMailDeadline(addr string, auth smtp.Auth, from string, to []string, msg []byte, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		conn.Close()
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	// Client.Close closes conn too; after a successful Quit it errors, which
	// is uninteresting either way.
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// maxExceptionLogBytes caps each ha.exceptions.log_file path (active + one
// rotated backup) so a script that keeps throwing can't fill /config.
const maxExceptionLogBytes = 5 << 20 // 5 MiB

// registerExceptionHandlers installs ha.exceptions.email and
// ha.exceptions.log_file. logsRoot confines log_file writes to the log
// directory; it may be nil (no log_dir configured), in which case log_file
// raises at registration.
func registerExceptionHandlers(L *lua.LState, t *lua.LTable, logsRoot *os.Root) {
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
		// Fail at registration, not at the first exception: a misconfigured
		// error sink that only reveals itself once something else is already
		// broken is worse than useless. The lexical guard catches the obvious
		// mistakes here; logsRoot still rejects symlink escapes at open time.
		if filepath.IsAbs(path) || strings.HasPrefix(filepath.Clean(path), "..") {
			L.RaiseError("ha.exceptions.log_file: path is relative to the log dir, got %q", path)
			return 0
		}
		if logsRoot == nil {
			L.RaiseError("ha.exceptions.log_file: no log_dir configured")
			return 0
		}
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
			// Create the parent dir so subdirectory paths work on a fresh
			// install before anything else has written there.
			if dir := filepath.Dir(path); dir != "" && dir != "." {
				if err := logsRoot.MkdirAll(dir, 0o755); err != nil {
					L.RaiseError("ha.exceptions.log_file: %v", err)
					return 0
				}
			}
			// Cap the file before appending, so it can't grow without bound.
			logwriter.RotateIfLarge(logsRoot, path, maxExceptionLogBytes)
			entry := fmt.Sprintf("---\n%s\n", body)
			if err := appendToRoot(logsRoot, path, []byte(entry)); err != nil {
				L.RaiseError("ha.exceptions.log_file: %v", err)
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
