package syslog

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		host     string
		priority int
		text     string
	}{
		// RouterOS 7.23.5 bytes, captured from AmonHen (state/syslog.md).
		{"RouterOS default", "script,info ha-lua test",
			"", -1, "script,info ha-lua test"},
		{"RouterOS syslog", "<30>Oct 17 19:24:25 AmonHen ha-lua test",
			"AmonHen", 30, "ha-lua test"},
		{"RouterOS syslog topics", "<30>Oct 17 19:24:34 AmonHen script,info ha-lua test",
			"AmonHen", 30, "script,info ha-lua test"},
		{"RouterOS iso8601", "<28>2026-10-17T19:24:45.159+02:00 AmonHen script,warning ha-lua warn",
			"AmonHen", 28, "script,warning ha-lua warn"},
		{"RouterOS link", "<30>Oct 17 19:14:53 AmonHen interface,info ether3 link down",
			"AmonHen", 30, "interface,info ether3 link down"},

		{"bsd padded day", "<13>Oct  7 09:01:02 nas kernel: oops\n",
			"nas", 13, "kernel: oops"},
		{"bsd no host", "<13>Oct 17 19:14:53 sshd[42]: accepted",
			"", 13, "sshd[42]: accepted"},
		{"rfc5424", "<165>1 2026-10-17T19:14:53+02:00 host app 42 ID47 - \ufeffhello",
			"host", 165, "hello"},
		{"rfc5424 sd", `<165>1 2026-10-17T19:14:53Z host app - - [ex@1 a="x\]y"][b@2 c="d"] hi`,
			"host", 165, "hi"},
		{"rfc5424 nil host", "<14>1 - - - - - - text", "", 14, "text"},
		{"rfc5424 no msg", "<14>1 2026-10-17T19:14:53Z host app - -", "host", 14, ""},
		{"pri only", "<0>bare", "", 0, "bare"},
		{"pri out of range", "<192>x", "", -1, "<192>x"},
		{"pri unterminated", "<30 x", "", -1, "<30 x"},
		{"empty", "", "", -1, ""},
		{"nul padded", "<30>hi\x00", "", 30, "hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := Parse([]byte(tt.raw))
			if msg.Host != tt.host || msg.Priority != tt.priority || msg.Text != tt.text {
				t.Errorf("Parse(%q) = host %q pri %d text %q, want %q %d %q",
					tt.raw, msg.Host, msg.Priority, msg.Text, tt.host, tt.priority, tt.text)
			}
			if msg.Raw != tt.raw {
				t.Errorf("Raw = %q, want %q", msg.Raw, tt.raw)
			}
		})
	}
}

func TestFacilitySeverity(t *testing.T) {
	msg := Parse([]byte("<30>x"))
	if msg.Facility() != 3 || msg.Severity() != 6 {
		t.Errorf("<30>: facility %d severity %d, want 3 6", msg.Facility(), msg.Severity())
	}
	msg = Parse([]byte("x"))
	if msg.Facility() != -1 || msg.Severity() != -1 {
		t.Errorf("no PRI: facility %d severity %d, want -1 -1", msg.Facility(), msg.Severity())
	}
}
