package sidekick

import (
	"net/url"
	"sort"
	"strings"
)

const (
	// DefaultCacheKeyQueries are the parameters that vary the cache key. Keying is
	// always safe; its only cost is more cache entries, so this list is generous:
	//
	//   - every public query var WordPress core registers (class-wp.php
	//     $public_query_vars, plus the sitemap vars), so plain-permalink sites, whose
	//     whole navigation is ?page_id=, ?cat=, ?m=..., keep caching;
	//   - the historical defaults (category is not a core var, but was always keyed);
	//   - ver and v, the asset cache-busters WordPress and themes append;
	//   - campaign labels, which have a handful of values per campaign.
	//
	// Plugin parameters are deliberately absent: a request carrying one is not cached
	// at all (see classifyQuery) until a site keys it explicitly.
	DefaultCacheKeyQueries = "p,page,paged,s,category,tag,author," +
		"m,posts,w,cat,withcomments,withoutcomments,search,exact,sentence,calendar,more,tb,pb," +
		"order,orderby,year,monthnum,day,hour,minute,second,name,category_name,feed,author_name," +
		"pagename,page_id,error,attachment,attachment_id,subpost,subpost_id,preview,robots,favicon," +
		"taxonomy,term,cpage,post_type,embed,sitemap,sitemap-subtype,sitemap-stylesheet," +
		"ver,v," +
		"utm_*,gad_*,mc_cid"

	// DefaultCacheIgnoreQueries are per-click or per-recipient tracking IDs. Keying them
	// would make every ad or email click a unique URL, so they are left out of the key
	// and the request shares the page's entry. Only values that cannot carry markup may
	// fill that entry (clickIDValueOK).
	DefaultCacheIgnoreQueries = "gclid,gbraid,wbraid,dclid,fbclid,msclkid,ttclid,twclid,li_fat_id," +
		"igshid,yclid,srsltid,_gl,_ga,mc_eid,_kx,_hsenc,_hsmi,mkt_tok"

	// maxClickIDValueLen bounds a click-ID value that may fill the cache.
	maxClickIDValueLen = 512
)

// queryClass is the single source of truth for how a request's query string affects
// caching. bypassReasonFor and buildCacheKeyFor both derive from the same value, so
// "this parameter is part of the key" and "this request may be cached" cannot drift
// apart — the keyDims pattern, applied to the query string.
//
// Every parameter PHP will see falls into exactly one class:
//
//	nocache_query          bypass, private
//	cache_key_queries      part of the key, every value
//	cache_ignore_queries   left out of the key; a value outside the click-ID
//	                       character set makes the request read-only
//	anything else          bypass: Sidekick cannot know whether it changes the page,
//	                       so it does not store the response
//
// A query that Go and PHP would read differently is bypassed as private, because no
// key computed from Go's view could describe what WordPress renders.
type queryClass struct {
	// Canonical is the keyed parameters, canonically encoded: PHP-normalized names,
	// sorted, every value in request order (url.Values.Encode).
	Canonical string
	// ReadOnly means a click-ID value failed clickIDValueOK. The request may be served
	// from cache but must never fill it.
	ReadOnly bool
	// Reason is bypassNone, or why the query alone forces a bypass.
	Reason bypassReason
	// Param names the parameter responsible for Reason or ReadOnly, for logging.
	Param string
}

// classifyQuery classifies a raw query string. See queryClass.
func (s *Sidekick) classifyQuery(rawQuery string) queryClass {
	if rawQuery == "" {
		return queryClass{}
	}

	if name, ok := s.matchNoCacheQuery(rawQuery); ok {
		return queryClass{Reason: bypassPrivate, Param: name}
	}

	parsed, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Go rejects what PHP accepts (';' inside a pair, a malformed escape).
		return queryClass{Reason: bypassPrivate, Param: "(unparseable query)"}
	}

	// Sorted so the outcome, including which parameter is reported, is deterministic.
	raws := make([]string, 0, len(parsed))
	for raw := range parsed {
		raws = append(raws, raw)
	}
	sort.Strings(raws)

	var qc queryClass
	keyed := url.Values{}
	owner := make(map[string]string, len(raws)) // PHP name -> raw name
	for _, raw := range raws {
		name, isArray := phpVarNameArray(raw)
		if name == "" {
			continue // PHP drops it, so it cannot change the page
		}
		if prev, ok := owner[name]; ok && prev != raw {
			// Two spellings PHP merges into one variable (product.cat, product_cat).
			// Which value WordPress sees depends on an order Go no longer has.
			return queryClass{Reason: bypassPrivate, Param: name}
		}
		owner[name] = raw

		switch {
		case isArray:
			qc.bypassFor(name)
		case s.isKeyedQuery(name):
			keyed[name] = parsed[raw]
		case s.isIgnoredQuery(name):
			for _, v := range parsed[raw] {
				if !clickIDValueOK(v) && !qc.ReadOnly {
					qc.ReadOnly = true
					if qc.Param == "" {
						qc.Param = name
					}
				}
			}
		case s.keyAllQueries():
			keyed[name] = parsed[raw]
		default:
			qc.bypassFor(name)
		}
	}

	qc.Canonical = keyed.Encode()
	return qc
}

// bypassFor records an unkeyed-parameter bypass, keeping the first parameter seen.
func (qc *queryClass) bypassFor(name string) {
	if qc.Reason == bypassNone {
		qc.Reason = bypassQuery
		qc.Param = name
	}
}

// cacheKeyQueryList returns the configured cache_key_queries. Nil means the module was
// constructed without Provision (as in unit tests); the default applies there too.
func (s *Sidekick) cacheKeyQueryList() []string {
	if s.CacheKeyQueries == nil {
		return strings.Split(DefaultCacheKeyQueries, ",")
	}
	return s.CacheKeyQueries
}

// cacheIgnoreQueryList returns the configured cache_ignore_queries, nil meaning default.
func (s *Sidekick) cacheIgnoreQueryList() []string {
	if s.CacheIgnoreQueries == nil {
		return strings.Split(DefaultCacheIgnoreQueries, ",")
	}
	return s.CacheIgnoreQueries
}

// isKeyedQuery reports whether name is explicitly listed in cache_key_queries. A bare
// "*" is not an explicit listing; see keyAllQueries.
func (s *Sidekick) isKeyedQuery(name string) bool {
	return matchesQueryList(s.cacheKeyQueryList(), name)
}

// keyAllQueries reports whether cache_key_queries contains "*": key every parameter that
// is not ignored.
func (s *Sidekick) keyAllQueries() bool {
	for _, q := range s.cacheKeyQueryList() {
		if q == "*" {
			return true
		}
	}
	return false
}

func (s *Sidekick) isIgnoredQuery(name string) bool {
	return matchesQueryList(s.cacheIgnoreQueryList(), name)
}

// matchesQueryList matches name against list entries: exact, or by prefix for an entry
// ending in '*' (the cache_key_cookies convention). A bare "*" matches nothing here.
func matchesQueryList(list []string, name string) bool {
	for _, entry := range list {
		if entry == "*" || entry == "" {
			continue
		}
		if prefix, ok := strings.CutSuffix(entry, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == entry {
			return true
		}
	}
	return false
}

// clickIDValueOK reports whether a click-ID value may fill a shared cache entry. Real
// IDs are base64url-like (_gl also uses '*'); none of these characters can open a tag
// or close a quoted HTML attribute or JS string, so a page rendered with such a value
// cannot carry injected markup to everyone else who is served that entry.
func clickIDValueOK(v string) bool {
	if len(v) > maxClickIDValueLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '.', c == '_', c == '~', c == '*', c == '=', c == '-':
		default:
			return false
		}
	}
	return true
}
