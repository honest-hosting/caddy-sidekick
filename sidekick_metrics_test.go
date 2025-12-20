package sidekick

import (
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		caddyfile   string
		expectError bool
		errorMsg    string
		validate    func(t *testing.T, s *Sidekick)
	}{
		{
			name: "valid metrics path",
			caddyfile: `sidekick {
				metrics /metrics/sidekick
				cache_dir /tmp/cache
			}`,
			expectError: false,
			validate: func(t *testing.T, s *Sidekick) {
				assert.Equal(t, "/metrics/sidekick", s.Metrics)
			},
		},
		{
			name: "metrics path with underscores and hyphens",
			caddyfile: `sidekick {
				metrics /metrics/sidekick-cache_stats
				cache_dir /tmp/cache
			}`,
			expectError: false,
			validate: func(t *testing.T, s *Sidekick) {
				assert.Equal(t, "/metrics/sidekick-cache_stats", s.Metrics)
			},
		},
		{
			name: "metrics disabled by default",
			caddyfile: `sidekick {
				cache_dir /tmp/cache
			}`,
			expectError: false,
			validate: func(t *testing.T, s *Sidekick) {
				assert.Equal(t, "", s.Metrics)
			},
		},
		{
			name: "invalid metrics path - no leading slash",
			caddyfile: `sidekick {
				metrics metrics/sidekick
				cache_dir /tmp/cache
			}`,
			expectError: true,
			errorMsg:    "metrics path must be an absolute path starting with /",
		},
		{
			name: "invalid metrics path - uppercase letters",
			caddyfile: `sidekick {
				metrics /Metrics/Sidekick
				cache_dir /tmp/cache
			}`,
			expectError: true,
			errorMsg:    "metrics path can only contain lowercase letters",
		},
		{
			name: "invalid metrics path - special characters",
			caddyfile: `sidekick {
				metrics /metrics@sidekick
				cache_dir /tmp/cache
			}`,
			expectError: true,
			errorMsg:    "metrics path can only contain lowercase letters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispenser := caddyfile.NewTestDispenser(tc.caddyfile)
			s := &Sidekick{}
			err := s.UnmarshalCaddyfile(dispenser)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorMsg != "" {
					assert.Contains(t, err.Error(), tc.errorMsg)
				}
			} else {
				require.NoError(t, err)
				if tc.validate != nil {
					tc.validate(t, s)
				}
			}
		})
	}
}

func TestMetricsEnvironmentVariable(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		expectError bool
		errorMsg    string
		expectedVal string
	}{
		{
			name:        "valid metrics path from env",
			envValue:    "/metrics/sidekick",
			expectError: false,
			expectedVal: "/metrics/sidekick",
		},
		{
			name:        "empty env var means disabled",
			envValue:    "",
			expectError: false,
			expectedVal: "",
		},
		{
			name:        "invalid path - no leading slash",
			envValue:    "metrics/sidekick",
			expectError: true,
			errorMsg:    "must be an absolute path starting with /",
		},
		{
			name:        "invalid path - uppercase",
			envValue:    "/Metrics/Sidekick",
			expectError: true,
			errorMsg:    "can only contain lowercase letters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Set environment variable
			t.Setenv("SIDEKICK_METRICS", tc.envValue)
			t.Setenv("SIDEKICK_CACHE_DIR", "/tmp/cache")

			// Create and provision sidekick
			s := &Sidekick{}
			ctx := caddy.Context{}
			err := s.Provision(ctx)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorMsg != "" {
					assert.Contains(t, err.Error(), tc.errorMsg)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedVal, s.Metrics)
			}
		})
	}
}
