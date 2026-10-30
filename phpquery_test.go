package sidekick

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPHPQueryNames pins the helpers to PHP's real behavior. Every expectation below
// was captured from PHP 8.2's parse_str, which shares $_GET's parser:
//
//	php -r 'parse_str($argv[1], $o); echo json_encode(array_keys($o));' -- '<query>'
func TestPHPQueryNames(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"rest.route=/", []string{"rest_route"}},
		{"rest+route=/", []string{"rest_route"}},
		{"rest%20route=/", []string{"rest_route"}},
		{"rest[route=/", []string{"rest_route"}},
		{"rest_route[]=/", []string{"rest_route"}},
		{"p=1;rest_route=/", []string{"p"}},
		{"%20rest_route=/", []string{"rest_route"}},
		{"rest%00x=/", []string{"rest"}},
		{"rest_route%00x=/", []string{"rest_route"}},
		{"a.b[c=1", []string{"a_b_c"}},
		{"a[b.c=1", []string{"a_b_c"}},
		{"rest%5Froute=/", []string{"rest_route"}},
		{"%zz=1", []string{"%zz"}},
		{"x[=1", []string{"x_"}},
		{"[a]=1", nil},
		{"rest_route", []string{"rest_route"}},
		{"=x&&rest_route=1", []string{"rest_route"}},
		{"REST_ROUTE=/", []string{"REST_ROUTE"}},
		{"", nil},
		{"a=1&b=2&a=3", []string{"a", "b", "a"}},
	}

	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			assert.Equal(t, tc.want, phpQueryNames(tc.query))
		})
	}
}

func TestPHPURLDecode(t *testing.T) {
	assert.Equal(t, "a b", phpURLDecode("a+b"))
	assert.Equal(t, "a_b", phpURLDecode("a%5Fb"))
	assert.Equal(t, "a_b", phpURLDecode("a%5fb"))
	assert.Equal(t, "%zz", phpURLDecode("%zz"), "malformed escapes are kept, not rejected")
	assert.Equal(t, "%4", phpURLDecode("%4"), "a truncated escape is kept")
	assert.Equal(t, "plain", phpURLDecode("plain"))
}
