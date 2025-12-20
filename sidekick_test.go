package sidekick

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
)

func TestUnmarshalCaddyfile_EmptyCacheKeyValues(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected func(*Sidekick)
	}{
		{
			name: "empty cache_key_queries",
			input: `sidekick {
				cache_key_queries ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "cache_key_queries should be empty when given empty string")
			},
		},
		{
			name: "empty cache_key_headers",
			input: `sidekick {
				cache_key_headers ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyHeaders, "cache_key_headers should be empty when given empty string")
			},
		},
		{
			name: "empty cache_key_cookies",
			input: `sidekick {
				cache_key_cookies ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyCookies, "cache_key_cookies should be empty when given empty string")
			},
		},
		{
			name: "mixed empty and non-empty values in cache_key_queries",
			input: `sidekick {
				cache_key_queries "page,,sort,"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"page", "sort"}, s.CacheKeyQueries, "should filter out empty values")
			},
		},
		{
			name: "non-empty cache_key_queries",
			input: `sidekick {
				cache_key_queries "page,sort,filter"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"page", "sort", "filter"}, s.CacheKeyQueries, "should preserve non-empty values")
			},
		},
		{
			name: "whitespace-only cache_key_headers",
			input: `sidekick {
				cache_key_headers "  ,  , "
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyHeaders, "should filter out whitespace-only values")
			},
		},
		{
			name: "multiple arguments with some empty",
			input: `sidekick {
				cache_key_cookies "session" "" "auth"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"session", "auth"}, s.CacheKeyCookies, "should filter out empty arguments")
			},
		},
		{
			name: "all three with empty values",
			input: `sidekick {
				cache_key_queries ""
				cache_key_headers ""
				cache_key_cookies ""
				cache_dir "/var/cache"
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "cache_key_queries should be empty")
				assert.Empty(t, s.CacheKeyHeaders, "cache_key_headers should be empty")
				assert.Empty(t, s.CacheKeyCookies, "cache_key_cookies should be empty")
				assert.Equal(t, "/var/cache", s.CacheDir, "other settings should work normally")
			},
		},
		{
			name: "whitespace with commas",
			input: `sidekick {
				cache_key_queries " , , "
				cache_key_headers "Accept, ,Content-Type"
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "should be empty when only whitespace")
				assert.Equal(t, []string{"Accept", "Content-Type"}, s.CacheKeyHeaders, "should filter empty but keep valid")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tt.input)
			s := &Sidekick{}

			err := s.UnmarshalCaddyfile(d)
			assert.NoError(t, err, "UnmarshalCaddyfile should not error")

			tt.expected(s)
		})
	}
}
