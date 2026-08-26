package mqtt

import "testing"

func TestValidateFilter(t *testing.T) {
	valid := []string{"a", "a/b", "a/+/b", "a/#", "#", "+", "+/tennis/#", "/a", "a//b"}
	for _, f := range valid {
		if err := ValidateFilter(f); err != nil {
			t.Errorf("ValidateFilter(%q) = %v, want nil", f, err)
		}
	}
	invalid := []string{"", "sport/#/ranking", "sport+", "sport/tennis#", "#/a"}
	for _, f := range invalid {
		if err := ValidateFilter(f); err == nil {
			t.Errorf("ValidateFilter(%q) = nil, want an error", f)
		}
	}
}

// The wildcard rules, including the ones that bite: '#' matching its own
// parent level, '+' not matching across levels, and neither reaching $SYS.
func TestMatch(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"zigbee2mqtt/dimmer/action", "zigbee2mqtt/dimmer/action", true},
		{"zigbee2mqtt/+/action", "zigbee2mqtt/dimmer/action", true},
		{"zigbee2mqtt/+/action", "zigbee2mqtt/a/b/action", false},
		{"zigbee2mqtt/#", "zigbee2mqtt/dimmer/action", true},
		{"zigbee2mqtt/#", "zigbee2mqtt", true},
		{"zigbee2mqtt/#", "zigbee2mqttx", false},
		{"sport/#", "sport/tennis/player1", true},
		{"sport/+", "sport/tennis/player1", false},
		{"+/+", "/finance", true},
		{"/+", "/finance", true},
		{"+", "/finance", false},
		{"#", "$SYS/broker/uptime", false},
		{"+/broker/uptime", "$SYS/broker/uptime", false},
		{"$SYS/#", "$SYS/broker/uptime", true},
		{"a/b", "a/b/c", false},
		{"a/b/c", "a/b", false},
	}
	for _, c := range cases {
		if got := Match(c.filter, c.topic); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.filter, c.topic, got, c.want)
		}
	}
}
