package lru

import (
	"os"
	"testing"
)

func TestLoadCapacityDefault(t *testing.T) {
	os.Unsetenv(EnvCapacity)
	if got := LoadCapacity(); got != DefaultCapacity {
		t.Errorf("LoadCapacity() = %d, want %d", got, DefaultCapacity)
	}
}

func TestLoadCapacityFromEnv(t *testing.T) {
	os.Setenv(EnvCapacity, "500")
	defer os.Unsetenv(EnvCapacity)

	if got := LoadCapacity(); got != 500 {
		t.Errorf("LoadCapacity() = %d, want 500", got)
	}
}

func TestLoadCapacityInvalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"negative", "-10"},
		{"zero", "0"},
		{"non-numeric", "abc"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				os.Setenv(EnvCapacity, tt.value)
				defer os.Unsetenv(EnvCapacity)
			} else {
				os.Unsetenv(EnvCapacity)
			}

			got := LoadCapacity()
			if got != DefaultCapacity {
				t.Errorf("LoadCapacity() = %d, want %d (default)", got, DefaultCapacity)
			}
		})
	}
}
