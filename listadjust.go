package sidekick

import (
	"fmt"
	"slices"
	"strings"
)

// List options (nocache, nocache_query, cache_key_queries, cache_ignore_queries,
// cache_key_headers, cache_key_cookies, cache_response_codes) accept either a plain
// list, which replaces the built-in default, or a list of adjustments to it:
//
//	cache_key_queries +filter_color,-utm_*
//
// "+x" appends x if absent and "-x" removes it, applied left to right. Adjustments keep
// a configuration current as the defaults evolve, where a copied plain list would
// freeze them. Honest Hosting's API resolves a site's adjustments against its site
// type before they reach Sidekick; this is the fallback for when there is no site type
// value, and for anyone configuring Sidekick directly.

// resolveListAdjustments returns values unchanged when it is a plain list, or def with
// the adjustments applied when every entry is one. A list mixing the two is an error.
func resolveListAdjustments(option string, values, def []string) ([]string, error) {
	adjustments := 0
	for _, v := range values {
		if isListAdjustment(v) {
			if strings.TrimSpace(v[1:]) == "" {
				return nil, fmt.Errorf("%s: adjustment %q names nothing", option, v)
			}
			adjustments++
		}
	}
	if adjustments == 0 {
		return values, nil
	}
	if adjustments != len(values) {
		return nil, fmt.Errorf("%s mixes +/- adjustments with plain entries (%q): use one form or the other",
			option, strings.Join(values, ","))
	}
	return applyListAdjustments(def, values), nil
}

func isListAdjustment(v string) bool {
	return strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-")
}

// applyListAdjustments applies "+x" / "-x" entries to a copy of base.
func applyListAdjustments(base, adjustments []string) []string {
	out := slices.Clone(base)
	for _, a := range adjustments {
		name := strings.TrimSpace(a[1:])
		switch a[0] {
		case '+':
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		case '-':
			out = slices.DeleteFunc(out, func(v string) bool { return v == name })
		}
	}
	if out == nil {
		out = []string{} // "explicitly empty", not "unset"
	}
	return out
}

// normalizeResponseCode turns a "2XX"-style wildcard into the single digit the status
// check matches on, keeping a leading +/- so "+4XX" stays an adjustment.
func normalizeResponseCode(code string) string {
	code = strings.TrimSpace(code)
	sign := ""
	if isListAdjustment(code) {
		sign, code = code[:1], strings.TrimSpace(code[1:])
	}
	if strings.Contains(code, "XX") && code != "" {
		code = code[:1]
	}
	return sign + code
}
