package main

import (
	"testing"

	"github.com/craftsman-apps/ambit/internal/console"
)

func TestKeySettingsFromEnv(t *testing.T) {
	for name, c := range map[string]struct {
		days, max string
		want      console.KeySettings
		fail      bool
	}{
		"defaults":      {"", "", console.KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}, false},
		"valid":         {"1", "90", console.KeySettings{Enabled: true, InactivityDays: 1, MaxDays: 90}, false},
		"upper bound":   {"365", "30", console.KeySettings{Enabled: true, InactivityDays: 365, MaxDays: 30}, false},
		"text defaults": {"abc", "xyz", console.KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}, false},
		"zero days":     {"0", "", console.KeySettings{}, true},
		"negative days": {"-30", "", console.KeySettings{}, true},
		"days too big":  {"366", "", console.KeySettings{}, true},
		"max 60":        {"", "60", console.KeySettings{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AMBIT_KEYS_INACTIVITY_DAYS", c.days)
			t.Setenv("AMBIT_KEYS_MAX_DAYS", c.max)
			got, err := keySettingsFromEnv()
			if (err != nil) != c.fail || (!c.fail && got != c.want) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}
