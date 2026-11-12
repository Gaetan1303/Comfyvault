package main

import (
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	good := map[string]time.Duration{
		"12h": 12 * time.Hour,
		"30d": 30 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
		"90m": 90 * time.Minute,
		"0d":  0,
	}
	for in, want := range good {
		got, err := parseAge(in)
		if err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "abc", "-3d", "1.5d", "-1h"} {
		if _, err := parseAge(in); err == nil {
			t.Errorf("parseAge(%q) should fail", in)
		}
	}
}
