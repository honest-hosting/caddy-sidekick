package sidekick

import (
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveListAdjustments(t *testing.T) {
	def := []string{"a", "b", "c"}

	got, err := resolveListAdjustments("opt", []string{"x", "y"}, def)
	require.NoError(t, err)
	assert.Equal(t, []string{"x", "y"}, got, "a plain list replaces the default")

	got, err = resolveListAdjustments("opt", []string{"+d", "-b", "+a"}, def)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "c", "d"}, got, "applied left to right; +existing is a no-op")

	got, err = resolveListAdjustments("opt", []string{"-a", "-b", "-c"}, def)
	require.NoError(t, err)
	assert.Equal(t, []string{}, got, "removing everything is explicitly empty, not unset")

	got, err = resolveListAdjustments("opt", []string{"-zzz"}, def)
	require.NoError(t, err)
	assert.Equal(t, def, got, "removing an absent entry is a no-op")

	got, err = resolveListAdjustments("opt", nil, def)
	require.NoError(t, err)
	assert.Nil(t, got, "unset stays unset")

	_, err = resolveListAdjustments("opt", []string{"+d", "e"}, def)
	assert.ErrorContains(t, err, "mixes")

	_, err = resolveListAdjustments("opt", []string{"+"}, def)
	assert.ErrorContains(t, err, "names nothing")

	assert.Equal(t, []string{"a", "b", "c"}, def, "the default itself is never modified")
}

func TestNormalizeResponseCode(t *testing.T) {
	for in, want := range map[string]string{
		"2XX": "2", " 301 ": "301", "+4XX": "+4", "-404": "-404", "- 5XX": "-5", "": "",
	} {
		assert.Equal(t, want, normalizeResponseCode(in), in)
	}
}

// TestProvision_ListAdjustments: every list option accepts adjustments from the
// Caddyfile, JSON or env, resolved against the module default.
func TestProvision_ListAdjustments(t *testing.T) {
	t.Setenv("SIDEKICK_CACHE_DIR", t.TempDir())

	t.Run("caddyfile", func(t *testing.T) {
		s := &Sidekick{}
		require.NoError(t, s.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`sidekick {
			nocache +/members
			nocache_query +myplugin_api
			cache_key_queries +filter_color -utm_*
			cache_ignore_queries +_branch_match_id -fbclid
			cache_key_headers +Accept-Language
			cache_key_cookies -wp-settings-*
			cache_response_codes +4XX -405
		}`)))
		s.WPMuPluginEnabled = false
		require.NoError(t, s.Provision(caddy.Context{}))

		assert.Equal(t, append(strings.Split(DefaultNoCache, ","), "/members"), s.NoCache)
		assert.Equal(t, append(strings.Split(DefaultNoCacheQuery, ","), "myplugin_api"), s.NoCacheQuery)
		assert.Contains(t, s.CacheKeyQueries, "filter_color")
		assert.Contains(t, s.CacheKeyQueries, "page_id")
		assert.NotContains(t, s.CacheKeyQueries, "utm_*")
		assert.Contains(t, s.CacheIgnoreQueries, "_branch_match_id")
		assert.NotContains(t, s.CacheIgnoreQueries, "fbclid")
		assert.Contains(t, s.CacheIgnoreQueries, "gclid")
		assert.Equal(t, []string{"Accept-Encoding", "Accept-Language"}, s.CacheKeyHeaders)
		assert.Equal(t, []string{"wordpress_logged_in_*", "wordpress_sec_*"}, s.CacheKeyCookies)
		assert.Equal(t, []string{"200", "404", "4"}, s.CacheResponseCodes)
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("SIDEKICK_NOCACHE", "+/members")
		t.Setenv("SIDEKICK_CACHE_KEY_QUERIES", "+filter_color")
		s := &Sidekick{}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.True(t, slices.Contains(s.NoCache, "/wp-admin") && slices.Contains(s.NoCache, "/members"))
		assert.Equal(t, append(strings.Split(DefaultCacheKeyQueries, ","), "filter_color"), s.CacheKeyQueries)
	})

	t.Run("mixed is a configuration error", func(t *testing.T) {
		s := &Sidekick{CacheKeyQueries: []string{"+filter_color", "page"}}
		assert.ErrorContains(t, s.Provision(caddy.Context{}), "cache_key_queries mixes")
	})

	t.Run("plain lists are unchanged", func(t *testing.T) {
		s := &Sidekick{NoCache: []string{"/only"}, CacheKeyQueries: []string{"*"}}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.Equal(t, []string{"/only"}, s.NoCache)
		assert.Equal(t, []string{"*"}, s.CacheKeyQueries)
	})
}
