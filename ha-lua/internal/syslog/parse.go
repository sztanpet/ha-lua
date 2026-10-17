// Package syslog receives UDP syslog datagrams and hands them to scripts.
// It is an input, not a log store: nothing is persisted.
package syslog

import (
	"strconv"
	"strings"
	"time"
)

// Message is one received datagram.
type Message struct {
	From       string // sender IP, from the UDP source address
	Host       string // hostname from the header; empty when the format has none
	Priority   int    // the <PRI> value, -1 without one
	Text       string // what follows the header, trimmed
	Raw        string // the datagram as received
	ReceivedAt time.Time
}

// Facility is Priority/8, or -1 without a <PRI>.
func (m Message) Facility() int {
	if m.Priority < 0 {
		return -1
	}
	return m.Priority / 8
}

// Severity is Priority%8, or -1 without a <PRI>.
func (m Message) Severity() int {
	if m.Priority < 0 {
		return -1
	}
	return m.Priority % 8
}

// Parse splits a datagram into header fields and text. It never rejects:
// whatever it cannot recognise stays in Text, so a sender with an odd
// format still reaches scripts whole.
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

// cutPriority takes a leading "<0>".."<191>".
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

// cutRFC5424 takes "1 TIMESTAMP HOST APP PROCID MSGID SD " after the PRI.
func cutRFC5424(s string, msg *Message) (string, bool) {
	fields := make([]string, 0, 6)
	rest := s
	for range 6 {
		field, after, ok := strings.Cut(rest, " ")
		if !ok {
			// A message-less record ends right after the last field.
			field, after = rest, ""
		}
		fields = append(fields, field)
		rest = after
	}
	if fields[0] != "1" {
		return s, false
	}
	if fields[2] != "-" {
		msg.Host = fields[2]
	}
	return skipStructuredData(rest), true
}

// skipStructuredData drops "-" or one or more "[id k="v"]" elements.
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

// elementEnd finds the ']' closing the element s starts with; a ']' inside
// a quoted, possibly escaped, param value does not count.
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

// cutTimestamp takes an RFC 3164 "Oct 17 19:14:53" or an ISO 8601
// "2026-10-17T19:14:53.159+02:00" and the space after it.
func cutTimestamp(s string) (string, bool) {
	const bsd = "Jan _2 15:04:05"
	if len(s) > len(bsd) && s[len(bsd)] == ' ' {
		if _, err := time.Parse(bsd, s[:len(bsd)]); err == nil {
			return s[len(bsd)+1:], true
		}
	}
	token, after, _ := strings.Cut(s, " ")
	if len(token) > 10 && token[10] == 'T' {
		if _, err := time.Parse(time.RFC3339Nano, token); err == nil {
			return after, true
		}
	}
	return s, false
}

// cutHost takes the hostname after a timestamp. A token ending in ':' or
// carrying '[' is a tag ("sshd[42]:"), which means the sender left the
// hostname out.
func cutHost(s string) (string, string, bool) {
	host, after, ok := strings.Cut(s, " ")
	if !ok || host == "" || strings.HasSuffix(host, ":") || strings.ContainsRune(host, '[') {
		return "", s, false
	}
	return host, after, true
}
