// Package syslog is an input, not a log store: nothing is persisted.
package syslog

import (
	"strconv"
	"strings"
	"time"
)

type Message struct {
	From       string
	Host       string
	Priority   int // -1 without a <PRI>
	Text       string
	Raw        string
	ReceivedAt time.Time
}

func (m Message) Facility() int {
	if m.Priority < 0 {
		return -1
	}
	return m.Priority / 8
}

func (m Message) Severity() int {
	if m.Priority < 0 {
		return -1
	}
	return m.Priority % 8
}

// Parse never rejects: whatever it cannot recognise stays in Text.
func Parse(raw []byte) Message {
	msg := Message{Raw: string(raw), Priority: -1}
	rest := msg.Raw

	if pri, after, ok := cutPriority(rest); ok {
		msg.Priority, rest = pri, after
		if after, ok := cutRFC5424(rest, &msg); ok {
			msg.Text = trim(strings.TrimPrefix(strings.TrimPrefix(after, " "), "\ufeff"))
			return msg
		}
	}
	if after, ok := cutTimestamp(rest); ok {
		rest = after
		if host, after, ok := cutHost(rest); ok {
			msg.Host, rest = host, after
		}
	}
	msg.Text = trim(rest)
	return msg
}

func trim(s string) string { return strings.Trim(s, " \t\r\n\x00") }

func cutPriority(s string) (int, string, bool) {
	if !strings.HasPrefix(s, "<") {
		return 0, s, false
	}
	end := strings.IndexByte(s, '>')
	if end < 2 || end > 4 {
		return 0, s, false
	}
	pri, err := strconv.Atoi(s[1:end])
	if err != nil || pri < 0 || pri > 191 {
		return 0, s, false
	}
	return pri, s[end+1:], true
}

// RFC 5424 after the <PRI>: 1 TIMESTAMP HOST APP PROCID MSGID SD MSG
func cutRFC5424(s string, msg *Message) (string, bool) {
	fields := strings.SplitN(s, " ", 7)
	if len(fields) < 6 || fields[0] != "1" || (fields[1] != "-" && !isRFC3339(fields[1])) {
		return s, false
	}
	if fields[2] != "-" {
		msg.Host = fields[2]
	}
	if len(fields) < 7 {
		return "", true
	}
	return skipStructuredData(fields[6]), true
}

func skipStructuredData(s string) string {
	if strings.HasPrefix(s, "-") {
		return s[1:]
	}
	for strings.HasPrefix(s, "[") {
		end := elementEnd(s)
		if end < 0 {
			return ""
		}
		s = s[end+1:]
	}
	return s
}

// A ']' inside a quoted param value does not close the element.
func elementEnd(s string) int {
	escaped, quoted := false, false
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ']' && !quoted:
			return i
		}
	}
	return -1
}

// RouterOS's iso8601 format puts an RFC 3339 timestamp in an RFC 3164 header.
func cutTimestamp(s string) (string, bool) {
	const bsd = "Jan _2 15:04:05"
	if len(s) > len(bsd) && s[len(bsd)] == ' ' {
		if _, err := time.Parse(bsd, s[:len(bsd)]); err == nil {
			return s[len(bsd)+1:], true
		}
	}
	if token, after, _ := strings.Cut(s, " "); isRFC3339(token) {
		return after, true
	}
	return s, false
}

func isRFC3339(s string) bool {
	if len(s) <= 10 || s[10] != 'T' {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// A tag ("sshd[42]:") where the hostname belongs means the sender left it out.
func cutHost(s string) (string, string, bool) {
	host, after, ok := strings.Cut(s, " ")
	if !ok || host == "" || strings.HasSuffix(host, ":") || strings.ContainsRune(host, '[') {
		return "", s, false
	}
	return host, after, true
}
