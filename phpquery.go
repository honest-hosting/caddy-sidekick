package sidekick

import "strings"

// Sidekick decides what to do with a request from Go's view of it, but WordPress acts on
// PHP's view of the same query string, and the two do not agree on what the parameter
// names are. The helpers below reproduce PHP's rules so that a check such as "is this a
// REST API request?" sees the same names WordPress will. Where Go and PHP differ:
//
//	input              Go url.ParseQuery            PHP $_GET
//	rest.route=/       "rest.route"                 "rest_route"  ('.' -> '_')
//	rest+route=/       "rest route"                 "rest_route"  (' ' -> '_')
//	rest[route=/       "rest[route"                 "rest_route"  (unmatched '[' -> '_')
//	rest_route[]=/     "rest_route[]"               "rest_route"  (array)
//
// The rules follow php-src main/php_variables.c (php_default_treat_data and
// php_register_variable_ex) as of PHP 8.

// phpQueryNames returns the top-level $_GET names PHP registers for a raw query string,
// in request order. Duplicates are kept.
func phpQueryNames(rawQuery string) []string {
	if rawQuery == "" {
		return nil
	}

	var names []string
	// arg_separator.input defaults to "&", so ';' is NOT a separator to PHP. Empty
	// tokens ("a=1&&b=2") are skipped, as strtok does.
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		name := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			name = pair[:i]
		}
		if n := phpVarName(phpURLDecode(name)); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// phpVarName normalizes an already-decoded variable name the way PHP does before
// registering it, and returns the top-level name ("" when PHP would ignore it).
func phpVarName(name string) string {
	n, _ := phpVarNameArray(name)
	return n
}

// phpVarNameArray is phpVarName that also reports whether PHP registers the variable
// as an array ("name[...]").
func phpVarNameArray(name string) (string, bool) {
	// The name is handled as a C string, so a decoded NUL ends it.
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimLeft(name, " ")

	b := []byte(name)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case ' ', '.':
			b[i] = '_'
		case '[':
			if strings.IndexByte(name[i+1:], ']') >= 0 {
				// "name[...]" registers an array under "name".
				return string(b[:i]), true
			}
			// An unmatched '[' is not an array. PHP 8 turns it, and every later
			// ' ', '.' or '[', into '_' and registers the whole name.
			for j := i; j < len(b); j++ {
				if b[j] == ' ' || b[j] == '.' || b[j] == '[' {
					b[j] = '_'
				}
			}
			return string(b), false
		}
	}
	return string(b), false
}

// phpURLDecode mirrors PHP's urldecode: '+' becomes a space and "%XX" with two hex
// digits becomes that byte. Unlike url.QueryUnescape it never fails; a malformed escape
// is kept literally, as PHP keeps it.
func phpURLDecode(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]):
			b.WriteByte(unhexDigit(s[i+1])<<4 | unhexDigit(s[i+2]))
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhexDigit(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
