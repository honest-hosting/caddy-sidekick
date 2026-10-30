package sidekick

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// muPluginPurgeBody is the exact request body the bundled mu-plugin sends for a post
// save. sidekick_purge_paths() passes 'body' => ['paths' => [...]] to wp_remote_post(),
// and WordPress's HTTP transport form-encodes an array body
// (wp-includes/Requests/src/Transport/Curl.php, http_build_query). Captured with:
//
//	php -r 'echo http_build_query(["paths" => ["/hello-world//*", "/category/news/*",
//	        "/feed/", "/hello-world//feed/"]], "", "&");'
const muPluginPurgeBody = "paths%5B0%5D=%2Fhello-world%2F%2F%2A&paths%5B1%5D=%2Fcategory%2Fnews%2F%2A" +
	"&paths%5B2%5D=%2Ffeed%2F&paths%5B3%5D=%2Fhello-world%2F%2Ffeed%2F"

func postPurge(t *testing.T, s *Sidekick, contentType, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", s.PurgePath, strings.NewReader(body))
	req.Header.Set(s.PurgeHeader, s.PurgeToken)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, restOrigin()))
	require.Equal(t, http.StatusOK, rec.Code)
}

func sidekickState(t *testing.T, s *Sidekick, target string) string {
	t.Helper()
	resp, _ := serve(t, s, restOrigin(), target)
	return resp.Header.Get(CacheHeaderName)
}

// TestMuPluginPurge_IsAFullFlushToday documents what a post save really does today.
// The form-encoded body is not JSON, the purge handler treats an unparseable body as
// "no paths", and "no paths" means flush everything. So the mu-plugin's path list
// never reaches Sidekick: every content change empties the cache.
//
// That full flush is what keeps content fresh after an edit (the home listing, widgets
// and archives a targeted list would miss), so it is pinned here deliberately. Moving
// the mu-plugin to targeted purges is a behavior change, not a fix.
func TestMuPluginPurge_IsAFullFlushToday(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	for _, page := range []string{"/hello-world/", "/unrelated-page/"} {
		require.Equal(t, "MISS", sidekickState(t, s, page))
		require.Equal(t, "HIT", sidekickState(t, s, page))
	}

	postPurge(t, s, "application/x-www-form-urlencoded", muPluginPurgeBody)

	assert.Equal(t, "MISS", sidekickState(t, s, "/hello-world/"))
	assert.Equal(t, "MISS", sidekickState(t, s, "/unrelated-page/"),
		"an unrelated page is purged too: the body never parsed, so everything was flushed")
}

// TestMuPluginPurge_PostPathPatterns covers the paths the mu-plugin builds, should
// they ever reach the targeted branch (a JSON body). The old "$link/*" pattern became
// "/hello-world//*" for the default /%postname%/ permalinks and never matched the
// post; the fixed plugin sends the exact path plus "/hello-world/*".
func TestMuPluginPurge_PostPathPatterns(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	warm := func() {
		for _, page := range []string{"/hello-world/", "/hello-world/2/", "/hello-world-2/"} {
			sidekickState(t, s, page)
			require.Equal(t, "HIT", sidekickState(t, s, page))
		}
	}

	warm()
	postPurge(t, s, "application/json", `{"paths":["/hello-world//*"]}`)
	assert.Equal(t, "HIT", sidekickState(t, s, "/hello-world/"), "the old double-slash pattern matches nothing")

	warm()
	postPurge(t, s, "application/json", `{"paths":["/hello-world/","/hello-world/*"]}`)
	assert.Equal(t, "MISS", sidekickState(t, s, "/hello-world/"))
	assert.Equal(t, "MISS", sidekickState(t, s, "/hello-world/2/"))
	assert.Equal(t, "HIT", sidekickState(t, s, "/hello-world-2/"), "a sibling with a shared prefix survives")
}
