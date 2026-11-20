// Package mqtt implements a pure-Go, zero-dependency MQTT 3.1.1 broker and
// multi-project hub for Open Fabrica. Each project gets an isolated broker with
// per-device credentials, topic ACLs, SQLite-backed retained messages, LWT, and
// publish hooks into SQLite tables, server functions, and the event bus.
package mqtt

import (
	"errors"
	"strings"
)

var (
	ErrInvalidTopic       = errors.New("invalid mqtt topic")
	ErrInvalidTopicFilter = errors.New("invalid mqtt topic filter")
)

// ValidatePublishTopic checks that topic is a valid concrete MQTT publish topic
// (non-empty and containing no '+' or '#' wildcards).
func ValidatePublishTopic(topic string) error {
	if topic == "" || len(topic) > 65535 {
		return ErrInvalidTopic
	}
	if strings.ContainsAny(topic, "+#\x00") {
		return ErrInvalidTopic
	}
	return nil
}

// ValidateTopicFilter validates an MQTT subscription filter according to MQTT 3.1.1
// section 4.7 (wildcards '+' and '#' must occupy whole levels, and '#' only at the end).
func ValidateTopicFilter(filter string) error {
	if filter == "" || len(filter) > 65535 || strings.ContainsRune(filter, '\x00') {
		return ErrInvalidTopicFilter
	}
	levels := strings.Split(filter, "/")
	for i, lvl := range levels {
		if strings.ContainsRune(lvl, '#') {
			if lvl != "#" || i != len(levels)-1 {
				return ErrInvalidTopicFilter
			}
		}
		if strings.ContainsRune(lvl, '+') && lvl != "+" {
			return ErrInvalidTopicFilter
		}
	}
	return nil
}

// isCaptureIdent reports whether name is a valid SQL/param identifier for {name} topic captures.
func isCaptureIdent(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func captureName(level string) (string, bool) {
	if len(level) >= 3 && level[0] == '{' && level[len(level)-1] == '}' {
		name := level[1 : len(level)-1]
		if isCaptureIdent(name) {
			return name, true
		}
	}
	return "", false
}

// ValidateRuleTopicFilter validates an ingest rule topic filter, allowing standard
// MQTT wildcards ('+', '#') as well as named single-level captures like
// "sensors/{zone}/{metric}" or "zigbee2mqtt/{device}".
func ValidateRuleTopicFilter(pattern string) error {
	if pattern == "" || len(pattern) > 65535 || strings.ContainsRune(pattern, '\x00') {
		return ErrInvalidTopicFilter
	}
	levels := strings.Split(pattern, "/")
	norm := make([]string, len(levels))
	for i, lvl := range levels {
		if strings.ContainsAny(lvl, "{}") {
			if _, ok := captureName(lvl); !ok {
				return ErrInvalidTopicFilter
			}
			norm[i] = "+"
			continue
		}
		norm[i] = lvl
	}
	return ValidateTopicFilter(strings.Join(norm, "/"))
}

// MatchTopicExtract matches topic against pattern (supporting '+', '#', and
// named single-level captures like '{device}') and returns captured segments.
func MatchTopicExtract(pattern, topic string) (bool, map[string]string) {
	if pattern == "" || topic == "" {
		return false, nil
	}
	fLevels := strings.Split(pattern, "/")
	if strings.HasPrefix(topic, "$") {
		first := fLevels[0]
		if first == "+" || first == "#" {
			return false, nil
		}
		if _, isCap := captureName(first); isCap {
			return false, nil
		}
	}
	if pattern == "#" {
		return true, nil
	}

	tLevels := strings.Split(topic, "/")
	var vars map[string]string

	for i := 0; i < len(fLevels); i++ {
		f := fLevels[i]
		if f == "#" {
			return true, vars
		}
		if i >= len(tLevels) {
			return false, nil
		}
		if name, isCap := captureName(f); isCap {
			if vars == nil {
				vars = make(map[string]string)
			}
			vars[name] = tLevels[i]
			continue
		}
		if f != "+" && f != tLevels[i] {
			return false, nil
		}
	}
	if len(fLevels) != len(tLevels) {
		return false, nil
	}
	return true, vars
}

// MatchTopic reports whether concrete topic matches MQTT filter, honoring '+'
// (single level), '#' (multi-level trailing), and the '$' system-topic rule
// (filters starting with '+' or '#' do not match topics starting with '$').
func MatchTopic(filter, topic string) bool {
	if filter == "" || topic == "" {
		return false
	}
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}
	if filter == "#" {
		return true
	}

	fLevels := strings.Split(filter, "/")
	tLevels := strings.Split(topic, "/")

	for i := 0; i < len(fLevels); i++ {
		f := fLevels[i]
		if f == "#" {
			return true
		}
		if i >= len(tLevels) {
			return false
		}
		if f != "+" && f != tLevels[i] {
			return false
		}
	}
	return len(fLevels) == len(tLevels)
}

// MatchAny reports whether topic matches any filter in a comma-separated ACL list.
func MatchAny(aclCSV, topic string) bool {
	for _, raw := range strings.Split(aclCSV, ",") {
		f := strings.TrimSpace(raw)
		if f != "" && MatchTopic(f, topic) {
			return true
		}
	}
	return false
}
