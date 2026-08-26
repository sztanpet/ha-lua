// Package mqtt is the daemon's MQTT client: one broker connection, shared by
// every script, with the topic-filter matching that decides which script sees
// which message.
package mqtt

import (
	"errors"
	"strings"
)

// ValidateFilter reports whether filter is a legal MQTT topic filter. It is
// called at script load time: a filter that can never match (a '#' in the
// middle, a '+' glued to a level) is a typo, and failing at load beats a
// subscription that silently never fires.
func ValidateFilter(filter string) error {
	if filter == "" {
		return errors.New("empty topic filter")
	}
	levels := strings.Split(filter, "/")
	for i, level := range levels {
		switch {
		case level == "#":
			if i != len(levels)-1 {
				return errors.New("'#' must be the last level of the filter")
			}
		case level == "+":
			// fine anywhere
		case strings.ContainsAny(level, "#+"):
			return errors.New("'#' and '+' must occupy a whole level, e.g. a/+/b")
		}
	}
	return nil
}

// Match reports whether topic matches filter, per the MQTT wildcard rules:
// '+' matches exactly one level, '#' matches the rest (including none), and
// neither matches a topic starting with '$' when the wildcard is at the root.
func Match(filter, topic string) bool {
	if filter == topic {
		return true
	}
	filterLevels := strings.Split(filter, "/")
	topicLevels := strings.Split(topic, "/")

	// $SYS and friends are server-internal: a subscription to "#" or "+/…"
	// must not sweep them up alongside the user's own topics.
	if strings.HasPrefix(topic, "$") && (filterLevels[0] == "#" || filterLevels[0] == "+") {
		return false
	}

	for i, level := range filterLevels {
		if level == "#" {
			return true // '#' also matches the parent level itself: a/# ~ a
		}
		if i >= len(topicLevels) {
			return false
		}
		if level != "+" && level != topicLevels[i] {
			return false
		}
	}
	return len(filterLevels) == len(topicLevels)
}
