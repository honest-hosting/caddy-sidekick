package sidekick

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
)

// VaryCookieHeader is the response header an application uses to declare that a
// response varies on the VALUE of named cookies, following LiteSpeed's
// X-LiteSpeed-Vary convention: "cookie=name1,cookie=name2". Standard Vary can only
// name the whole Cookie header, which would split the cache per session.
//
// The header is consumed here and never sent downstream.
const VaryCookieHeader = "X-Sidekick-Vary"

// DefaultVaryIndexMax bounds how many URLs may have a learned vary spec.
const DefaultVaryIndexMax = 100000

// varySpec is what an origin response declared it varies on, beyond the
// dimensions Sidekick always keys on (host, path, configured query/headers/cookies)
// and Accept-Encoding, which Sidekick negotiates itself.
type varySpec struct {
	Headers []string // canonical MIME header names, sorted, unique
	Cookies []string // cookie names, sorted, unique
}

func (v varySpec) empty() bool { return len(v.Headers) == 0 && len(v.Cookies) == 0 }

func (v varySpec) equal(o varySpec) bool {
	return slices.Equal(v.Headers, o.Headers) && slices.Equal(v.Cookies, o.Cookies)
}

// variantKey derives the storage key of this request's variant from the base key.
// An absent header or cookie is encoded distinctly from an empty value.
func (v varySpec) variantKey(base string, r *http.Request) string {
	h := md5.New()
	h.Write([]byte(base))
	h.Write([]byte{0, 'v'})
	for _, name := range v.Headers {
		h.Write([]byte{0, 'h'})
		h.Write([]byte(name))
		if values, ok := r.Header[name]; ok {
			h.Write([]byte{'='})
			h.Write([]byte(strings.TrimSpace(strings.Join(values, ","))))
		}
	}
	for _, name := range v.Cookies {
		h.Write([]byte{0, 'c'})
		h.Write([]byte(name))
		if c, err := r.Cookie(name); err == nil {
			h.Write([]byte{'='})
			h.Write([]byte(c.Value))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// varyDecision is the outcome of reading an origin response's vary declarations.
type varyDecision struct {
	spec varySpec
	// passthrough holds Vary tokens Sidekick cannot key on (Cookie, *). Such a
	// response is not stored, and the tokens are kept in the downstream Vary so
	// other caches see the origin's statement.
	passthrough []string
}

func (d varyDecision) uncacheable() bool { return len(d.passthrough) > 0 }

// parseResponseVary reads Vary and X-Sidekick-Vary from an origin response.
func parseResponseVary(hdr http.Header) varyDecision {
	var d varyDecision
	for _, line := range hdr.Values("Vary") {
		for _, token := range strings.Split(line, ",") {
			token = strings.TrimSpace(token)
			switch {
			case token == "":
			case token == "*":
				d.passthrough = appendUnique(d.passthrough, "*")
			case strings.EqualFold(token, "Accept-Encoding"):
				// Negotiated by Sidekick itself (it stores one representation and
				// encodes per client), so it never splits the key.
			case strings.EqualFold(token, "Cookie"):
				d.passthrough = appendUnique(d.passthrough, "Cookie")
			case validToken(token):
				d.spec.Headers = appendUnique(d.spec.Headers, textproto.CanonicalMIMEHeaderKey(token))
			}
		}
	}
	for _, line := range hdr.Values(VaryCookieHeader) {
		for _, item := range strings.Split(line, ",") {
			name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
			if ok && strings.EqualFold(strings.TrimSpace(name), "cookie") {
				if value = strings.TrimSpace(value); validToken(value) {
					d.spec.Cookies = appendUnique(d.spec.Cookies, value)
				}
			}
		}
	}
	slices.Sort(d.spec.Headers)
	slices.Sort(d.spec.Cookies)
	return d
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// validToken accepts an RFC 9110 token (header names, cookie names).
func validToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("\"(),/:;<=>?@[\\]{}", c) >= 0 {
			return false
		}
	}
	return true
}

// varyIndex remembers, per base key, the vary spec its responses declared, so a
// later request can be looked up under the right variant key. It is a bounded
// in-memory hint: after a restart the first request for a URL misses and the spec
// is learned again from the origin's response.
type varyIndex struct {
	mu      sync.RWMutex
	entries map[string]varySpec
	max     int
}

func newVaryIndex(max int) *varyIndex {
	if max <= 0 {
		max = DefaultVaryIndexMax
	}
	return &varyIndex{entries: make(map[string]varySpec), max: max}
}

func (x *varyIndex) get(base string) (varySpec, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	spec, ok := x.entries[base]
	return spec, ok
}

// set records a non-empty spec, or forgets the URL when spec is empty. It reports
// false when the index is full and the spec could not be recorded; the caller must
// then not store the variant, because no later request could find it.
func (x *varyIndex) set(base string, spec varySpec) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if spec.empty() {
		delete(x.entries, base)
		return true
	}
	if _, exists := x.entries[base]; !exists && len(x.entries) >= x.max {
		return false
	}
	x.entries[base] = spec
	return true
}

func (x *varyIndex) clear() {
	x.mu.Lock()
	x.entries = make(map[string]varySpec)
	x.mu.Unlock()
}

// respectVaryEnabled reports whether origin Vary declarations split the cache. A nil
// RespectVary means the module was built without Provision (unit tests): off.
func (s *Sidekick) respectVaryEnabled() bool {
	return s.RespectVary != nil && *s.RespectVary && s.varyIdx != nil
}

// varyCDNNoStoreEnabled reports whether varied responses are withheld from shared
// CDNs that cannot key on the varied dimension.
func (s *Sidekick) varyCDNNoStoreEnabled() bool {
	return s.VaryCDNNoStore == nil || *s.VaryCDNNoStore
}

// lookupKey returns the key a request should be served from, adding any learned
// vary dimensions to dims so downstream headers describe them.
func (s *Sidekick) lookupKey(base string, r *http.Request, dims keyDims) (string, keyDims) {
	if !s.respectVaryEnabled() {
		return base, dims
	}
	spec, ok := s.varyIdx.get(base)
	if !ok {
		return base, dims
	}
	dims.Vary = spec
	return spec.variantKey(base, r), dims
}
