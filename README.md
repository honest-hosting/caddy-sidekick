# Caddy Sidekick

Lightning-fast server side caching Caddy module for PHP applications, with an emphasis on WordPress

## Features

### Performance Optimizations
- **Buffer Pooling**: Uses `sync.Pool` for efficient memory management
- **Automatic Compression**: Stores compressed versions (gzip, brotli, zstd) when beneficial
- **Streaming to Disk**: Large responses stream directly to disk instead of buffering in memory
- **304 Not Modified Support**: Handles conditional requests with ETag and Last-Modified headers
- **Pre-compiled Regex**: Patterns compiled once during initialization

### Advanced Cache Management
- **Configurable Cache Keys**: Include query parameters, headers, and cookies in cache key generation
- **Size Management**: Fine-grained control over memory usage and cache limits
- **Selective Caching**: Path prefixes, regex patterns, and response codes
- **Purge API**: Secure cache invalidation endpoint

### WordPress Integration
- **Automatic mu-plugin Deployment**: Manages WordPress must-use plugins for cache purging and URL rewriting
- **Checksum Verification**: Ensures mu-plugin integrity with SHA-256 checksums
- **Smart Directory Management**: Creates directories as needed with parent directory validation

## Installation

### Building with xcaddy

```bash
xcaddy build --with github.com/honest-hosting/caddy-sidekick
```

## Configuration

### Environment Variables

All environment variables use the `SIDEKICK_` prefix for namespace isolation:

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `SIDEKICK_CACHE_DIR` | Cache storage directory | `/var/www/html/wp-content/cache` |
| `SIDEKICK_METRICS` | Enable metrics and set admin API path (e.g., `/metrics/sidekick`) | _(disabled)_ |
| `SIDEKICK_CACHE_RESPONSE_CODES` | HTTP status codes to cache (comma-separated) | `2XX,301,302` |
| `SIDEKICK_NOCACHE` | Path prefixes to bypass cache (comma-separated, uses prefix matching) | `/wp-admin,/wp-json` |
| `SIDEKICK_NOCACHE_HOME` | Skip caching home page | `false` |
| `SIDEKICK_NOCACHE_REGEX` | Regex pattern for paths to bypass | `\.(jpg\|jpeg\|png\|gif\|ico\|css\|js\|svg\|woff\|woff2\|ttf\|eot\|otf\|mp4\|webm\|mp3\|ogg\|wav\|pdf\|zip\|tar\|gz\|7z\|exe\|doc\|docx\|xls\|xlsx\|ppt\|pptx)$` |
| `SIDEKICK_CACHE_TTL` | Cache time-to-live in seconds | `300` |
| `SIDEKICK_PURGE_HEADER` | HTTP header name for purge token | `X-Sidekick-Purge` |
| `SIDEKICK_PURGE_PATH` | API endpoint for cache purging (absolute path, only a-z0-9-_/ allowed) | `/__sidekick/purge` |
| `SIDEKICK_PURGE_URL` | Optional custom URL for cache purging (e.g., `https://api.example.com`) | _(empty)_ |
| `SIDEKICK_PURGE_TOKEN` | Secret token for purge authentication (required when cache is enabled) | `dead-beef` |
| `SIDEKICK_CACHE_MEMORY_ITEM_MAX_SIZE` | Max size for single item in memory (e.g., `4MB`, `0` = disabled, `-1` = unlimited) | `4MB` |
| `SIDEKICK_CACHE_MEMORY_MAX_SIZE` | Total memory cache size limit (e.g., `128MB`, `0` = disabled, `-1` = unlimited) | `128MB` |
| `SIDEKICK_CACHE_MEMORY_MAX_PERCENT` | Memory cache as % of RAM (1-100, `0` = disabled, `-1` = unlimited). Mutually exclusive with `SIDEKICK_CACHE_MEMORY_MAX_SIZE` | _(none)_ |
| `SIDEKICK_CACHE_MEMORY_MAX_COUNT` | Max number of items in memory cache | `32768` |
| `SIDEKICK_CACHE_MEMORY_STREAM_TO_DISK_SIZE` | Size threshold for streaming to disk (e.g., `10MB`, `0` = disabled) | `10MB` |
| `SIDEKICK_CACHE_DISK_ITEM_MAX_SIZE` | Max size for any cached item on disk (e.g., `100MB`, `0` = disabled, `-1` = unlimited) | `100MB` |
| `SIDEKICK_CACHE_DISK_MAX_SIZE` | Total disk cache size limit (e.g., `10GB`, `0` = disabled, `-1` = unlimited) | `10GB` |
| `SIDEKICK_CACHE_DISK_MAX_PERCENT` | Disk cache as % of available space (1-100, `0` = disabled, `-1` = unlimited). Mutually exclusive with `SIDEKICK_CACHE_DISK_MAX_SIZE` | _(none)_ |
| `SIDEKICK_CACHE_DISK_MAX_COUNT` | Max number of items in disk cache (`-1` = unlimited, `0` = disabled) | `100000` |
| `SIDEKICK_CACHE_KEY_HEADERS` | Headers to include in cache key (comma-separated) | `Accept-Encoding` |
| `SIDEKICK_CACHE_KEY_QUERIES` | Query parameters to include in cache key (comma-separated, use `*` for all) | `p,page,paged,s,category,tag,author` |
| `SIDEKICK_CACHE_KEY_COOKIES` | Cookies to include in cache key (comma-separated, supports wildcards with `*`) | `wordpress_logged_in_*,wordpress_sec_*,wp-settings-*` |
| `SIDEKICK_WP_MU_PLUGIN_ENABLED` | Enable automatic WordPress mu-plugin management | `true` |
| `SIDEKICK_WP_MU_PLUGIN_DIR` | Directory for WordPress mu-plugins | `/var/www/html/wp-content/mu-plugins` |

**Note:** When either memory or disk cache is enabled, all purge-related options (`SIDEKICK_PURGE_HEADER`, `SIDEKICK_PURGE_PATH`, `SIDEKICK_PURGE_TOKEN`) are required to be set. The `SIDEKICK_PURGE_URL` is optional and only needed if you want to use a custom URL for cache purging instead of the WordPress site URL.

### Quick Start

Minimal configuration for a WordPress site:

```caddyfile
{
    order sidekick before rewrite
}

example.com {
    sidekick {
        # Optional: Enable metrics collection (disabled by default to save resources)
        # metrics /metrics/sidekick
        
        cache_dir /var/www/cache
        cache_ttl 3600
        
        purge_path /__sidekick/purge
        purge_header X-Sidekick-Purge
        purge_token "change-this-secret"
    }
    
    root * /var/www/html
    php_server
    file_server
}
```

### Complete Caddyfile Example

Full configuration with all options for a production WordPress site:

```caddyfile
{
    # Global options
    # Enable admin API for metrics (comment out to disable)
    admin localhost:2019
    
    # FrankenPHP configuration
    frankenphp
    
    # Module ordering
    order php_server before file_server
    order php before file_server
    order sidekick before rewrite
    order request_header before sidekick
}

example.com {
    # Enable Sidekick caching
    sidekick {
        # Enable metrics collection and expose on admin API
        # Metrics will be available at:
        # - Admin API: http://localhost:2019/metrics/sidekick (detailed sidekick metrics)
        # - Caddy metrics: http://localhost:2019/metrics (includes sidekick metrics in Prometheus format)
        metrics /metrics/sidekick
        
        # Cache storage location
        cache_dir /var/www/cache
        
        # Cache TTL in seconds (default: 300)
        cache_ttl 3600
        
        # HTTP status codes to cache
        cache_response_codes 200 301 302
        
        # Paths to bypass cache (uses prefix matching, so /wp-admin matches /wp-admin/*)
        nocache /wp-admin /wp-json /wp-login.php
        
        # Don't cache home page (optional)
        nocache_home false
        
        # Regex for file types to bypass
        # Exclude large media files from cache
        nocache_regex "\\.(mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe)$"
        
        # How downstream cache headers are chosen (see "Downstream Cacheability").
        # "preserve" (default) picks headers from the reason a request bypassed;
        # "nostore" restores the legacy blanket no-store on every non-HIT path.
        bypass_cache_control preserve
        
        # Paths whose bytes cannot vary by cookie (see "Shared cache safety" below).
        # These are exempt from the WordPress login-cookie bypass and from cookie
        # cache keying, so one shared entry serves logged-in and anonymous visitors.
        # Default shown; widen only if the site has no download-gating plugin.
        static_asset_regex "\\.(css|js|mjs|map|jpg|jpeg|png|gif|webp|avif|svg|ico|woff|woff2|ttf|otf|eot)$"
        
        # Purge endpoint configuration (required when cache is enabled)
        purge_path /__sidekick/purge
        purge_header X-Sidekick-Purge
        purge_token "your-secret-token-here"  # CHANGE THIS!
        
        # Optional: Use custom URL for cache purging (e.g., when behind a proxy)
        # purge_url https://api.example.com
        
        # Memory cache limits
        cache_memory_item_max_size 4MB
        cache_memory_max_size 128MB
        cache_memory_max_count 32768
        cache_memory_stream_to_disk_size 10MB
        
        # Disk cache limits
        cache_disk_item_max_size 100MB
        cache_disk_max_size 10GB
        cache_disk_max_count 100000
        
        # Range request support (see "Range Requests" below).
        # On a cache miss carrying a Range header, fetch the full representation to
        # populate the cache, then serve the requested range from it. Without this,
        # range-requested URLs (video, audio, large downloads) never become cacheable.
        range_fill true
        # Abandon a fill whose body exceeds this and relay the response whole.
        # Defaults to cache_disk_item_max_size. Use -1 for no limit.
        range_fill_max_size 100MB
        # How long a concurrent request waits for an in-flight fill of the same
        # object before going to the origin itself (default 2s).
        range_fill_collapse_wait 2s
        
        # Largest body considered for compression before storing (default 1MB).
        # Use -1 for unlimited. Above this, gzip/brotli cost more CPU and transient
        # memory than the disk space they save. Bodies whose Content-Type is already
        # compressed (video, audio, most images, archives, PDF, woff) are skipped
        # regardless of size.
        compress_max_size 1MB
        
        # Cache key customization (defaults shown below if omitted)
        # Note: Set to "" (empty string) to disable, but this is not recommended
        cache_key_queries page sort filter     # Default: p,page,paged,s,category,tag,author
        cache_key_headers Accept-Language      # Default: Accept-Encoding
        cache_key_cookies session_id user_*    # Default: wordpress_logged_in_*,wordpress_sec_*,wp-settings-*
                                                # Supports wildcards with * for prefix matching
        
        # WordPress mu-plugin management
        wp_mu_plugin_enabled true
        wp_mu_plugin_dir /var/www/html/wp-content/mu-plugins
    }
    
    # Set document root
    root * /var/www/html
    
    # PHP handling with FrankenPHP
    php_server
    
    # Static file serving
    file_server
    
    # Compression
    encode gzip
    
    # Optional: Add custom headers
    header {
        X-Frame-Options "SAMEORIGIN"
        X-Content-Type-Options "nosniff"
        X-XSS-Protection "1; mode=block"
    }
    
    # Optional: Logging
    log {
        output file /var/log/caddy/access.log
        format console
    }
    
    # Handle errors
    handle_errors {
        @404 expression {http.error.status_code} == 404
        handle @404 {
            header Content-Type "text/html; charset=utf-8"
            respond "<!DOCTYPE html><html><head><title>404 Not Found</title></head><body><h1>404 - Page Not Found</h1></body></html>" 404
        }
        
        respond "{http.error.status_code} {http.error.status_text}"
    }
}
```

### JSON Configuration Example

```json
{
  "admin": {
    "listen": "localhost:2019"
  },
  "apps": {
    "http": {
      "servers": {
        "srv0": {
          "metrics": {},
          "listen": [":443"],
          "routes": [
            {
              "match": [
                {
                  "host": ["example.com"]
                }
              ],
              "handle": [
                {
                  "handler": "subroute",
                  "routes": [
                    {
                      "handle": [
                        {
                          "handler": "sidekick",
                          "metrics": "/metrics/sidekick",
                          "cache_dir": "/var/www/cache",
                          "cache_ttl": 3600,
                          "cache_response_codes": ["200", "301", "302"],
                          "nocache": ["/wp-admin", "/wp-json", "/wp-login.php"],
                          "nocache_home": false,
                          "nocache_regex": "\\.(mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe)$",
                          "purge_path": "/__sidekick/purge",
                          "purge_url": "",
                          "purge_header": "X-Sidekick-Purge",
                          "purge_token": "your-secret-token-here",
                          "cache_memory_item_max_size": 4194304,
                          "cache_memory_max_size": 134217728,
                          "cache_memory_max_count": 32768,
                          "cache_memory_stream_to_disk_size": 10485760,
                          "cache_disk_item_max_size": 104857600,
                          "cache_disk_max_size": 10737418240,
                          "cache_disk_max_count": 100000,
                          "cache_key_queries": ["page", "sort", "filter"],
                          "cache_key_headers": ["Accept-Language"],
                          "cache_key_cookies": ["wordpress_logged_in_*"],
                          "wp_mu_plugin_enabled": true,
                          "wp_mu_plugin_dir": "/var/www/html/wp-content/mu-plugins"
                        }
                      ]
                    },
                    {
                      "handle": [
                        {
                          "handler": "rewrite",
                          "uri": "{http.matchers.file.relative}"
                        }
                      ],
                      "match": [
                        {
                          "file": {
                            "try_files": ["{http.request.uri.path}", "{http.request.uri.path}/", "index.php"]
                          }
                        }
                      ]
                    },
                    {
                      "handle": [
                        {
                          "handler": "reverse_proxy",
                          "transport": {
                            "protocol": "fastcgi",
                            "split_path": [".php"]
                          },
                          "upstreams": [
                            {
                              "dial": "localhost:9000"
                            }
                          ]
                        }
                      ],
                      "match": [
                        {
                          "path": ["*.php"]
                        }
                      ]
                    },
                    {
                      "handle": [
                        {
                          "handler": "file_server",
                          "root": "/var/www/html"
                        }
                      ]
                    }
                  ]
                }
              ]
            }
          ],
          "errors": {
            "routes": [
              {
                "match": [
                  {
                    "expression": "{http.error.status_code} == 404"
                  }
                ],
                "handle": [
                  {
                    "handler": "headers",
                    "response": {
                      "set": {
                        "Content-Type": ["text/html; charset=utf-8"]
                      }
                    }
                  },
                  {
                    "handler": "static_response",
                    "status_code": 404,
                    "body": "<!DOCTYPE html><html><head><title>404 Not Found</title></head><body><h1>404 - Page Not Found</h1></body></html>"
                  }
                ]
              }
            ]
          },
          "logs": {
            "logger_names": {
              "*": "default"
            }
          }
        }
      }
    },
    "logging": {
      "logs": {
        "default": {
          "writer": {
            "output": "file",
            "filename": "/var/log/caddy/access.log"
          },
          "encoder": {
            "format": "console"
          }
        }
      }
    }
  }
}
```

## Cache Management

### Purging Cache

The purge API only accepts POST requests with an optional JSON body specifying paths to purge.

#### Purge all cache (empty body or no body):
```bash
curl -X POST https://example.com/__sidekick/purge \
  -H "X-Sidekick-Purge: your-secret-token"
```

#### Purge specific paths (JSON body):
```bash
curl -X POST https://example.com/__sidekick/purge \
  -H "X-Sidekick-Purge: your-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["/blog/post-1", "/blog/post-2", "/products/*"]}'
```

#### Purge with wildcard patterns:
```bash
curl -X POST https://example.com/__sidekick/purge \
  -H "X-Sidekick-Purge: your-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["/blog/*", "/products/category-*", "/api/v1/*"]}'
```

### WordPress mu-plugins

Sidekick automatically manages WordPress must-use plugins when `wp_mu_plugin_enabled` is set to `true` (default). These plugins provide:

1. **Content Cache Purge**: Automatically purges cache when posts are updated
   - Uses `SIDEKICK_PURGE_URL` if set to send purge requests to a custom URL (e.g., `https://api.example.com`)
   - Falls back to WordPress `get_site_url()` if `SIDEKICK_PURGE_URL` is not set
   - Constructs the full purge URL as `{SIDEKICK_PURGE_URL}{SIDEKICK_PURGE_PATH}` or `{get_site_url()}{SIDEKICK_PURGE_PATH}`
2. **Force URL Rewrite**: Ensures proper URL handling for WordPress

The mu-plugins are:
- Automatically deployed on startup if not present
- Updated if checksums don't match (ensuring latest version)
- Removed if the feature is disabled and files match expected checksums
- Only deployed if the parent directory exists (with warnings otherwise)

To disable automatic mu-plugin management:
```caddyfile
sidekick {
    wp_mu_plugin_enabled false
}
```

Or via environment variable:
```bash
SIDEKICK_WP_MU_PLUGIN_ENABLED=false
```

## Cache Key Configuration

### Default Cache Key Components

By default, Sidekick includes the following in cache keys:

- **Query Parameters**: `p`, `page`, `paged`, `s`, `category`, `tag`, `author` (common WordPress parameters)
- **Headers**: `Host` (to prevent cache pollution/poisoning), `Accept-Encoding` (to vary cache by compression support)
- **Cookies**: `wordpress_logged_in_*`, `wordpress_sec_*`, `wp-settings-*` (WordPress session cookies, with wildcard support)

### Customizing Cache Keys

You can override these defaults in your Caddyfile:

```caddyfile
sidekick {
    # Include all query parameters in cache key
    cache_key_queries *
    
    # Include specific headers
    cache_key_headers Accept-Language User-Agent
    
    # Include cookies with wildcard patterns
    cache_key_cookies session_* user_pref_*
}
```

**Important**: Setting any of these to an empty string (`""`) will disable that component entirely, which is not recommended as it may cause cache pollution. If you see warnings about empty cache key options, consider if you really want to disable them.

### Cookie Wildcard Matching

The `cache_key_cookies` option supports wildcard patterns using `*` for prefix matching:
- `wordpress_logged_in_*` matches any cookie starting with `wordpress_logged_in_`
- `session_*` matches `session_id`, `session_token`, etc.
- Exact names (without `*`) only match that specific cookie

## Range Requests

Sidekick stores exactly one representation per cache key: the full `200` response.
Range requests are answered by reading a window out of that stored representation via
`http.ServeContent`, which handles range parsing, `If-Range`, single and multipart
ranges, `Content-Range`, `416` and `Accept-Ranges`.

**A partial response is never stored.** This is a hard invariant, not a preference.
The cache key does not include `Range`, so a stored `206` would become *the* entry for
that URL and be served to every subsequent requester as though it were the whole
object.

Two halves make this work:

- **Range-aware hits.** A `Range` request against a cached, identity-encoded `200` is
  served as a correct `206` straight from cache. Compressed entries are excluded —
  byte offsets are meaningless against a compressed representation — and are served
  whole instead.
- **Range fill (`range_fill`).** Browsers fetch video almost exclusively via `Range`,
  including the opening `bytes=0-` probe, so without this a video URL would never
  produce a cacheable response and would miss forever. On a miss carrying a `Range`,
  Sidekick re-issues the request upstream with `Range` stripped, captures the full
  body for the cache, and serves the client's requested range from that capture. Every
  later range request for the URL is then a plain cache hit.

If a fill turns out not to be a cacheable `200`, or the body outgrows
`cache_disk_item_max_size` / `range_fill_max_size`, the fill is abandoned and the
response is streamed to the client in full. The client always receives a complete,
correct response; only the cache fill is lost.

Note that the first range request for a cold object costs a full read of that object
from the origin. Against a local `file_server` that is cheap. Set `range_fill false`
if your origin makes it expensive.

### Streaming From Disk

When the stored bytes can go to the client unchanged, Sidekick serves them from an
open file rather than reading the entry into memory first. A cached 34MB video is
therefore never fully resident in RAM, no matter how many viewers are streaming it —
which is what makes `cache_memory_stream_to_disk_size` describe real behavior on both
the write and the read side.

The buffered path is still used when the body has to be transformed: an
identity-encoded entry being compressed on the fly for a client that asked for gzip or
brotli, or an entry that was compressed on disk and must be decompressed. Cached
redirects and error statuses also take the buffered path, since they need their own
status code rather than the `200`/`206` that streaming produces.

Nothing about this is configurable — it is chosen automatically per request based on
whether a transform is needed.

### Request Collapsing

A range fill reads the whole object, so several viewers seeking into the same cold
video at once would otherwise mean several full reads. The first request for a key
becomes the leader and performs the fill; concurrent range requests for that key wait
up to `range_fill_collapse_wait` and are then served from the resulting cache entry.

Collapsing is best-effort and degrades open. If the wait expires, the client
disconnects, or the leader produced nothing cacheable, the follower falls through to a
plain pass-through of its original range request, which the origin answers directly.
A follower never blocks indefinitely and never fails a request that would otherwise
have succeeded — the worst case is the behavior you had before collapsing existed.

Only range fills are collapsed. Ordinary misses are cheap and keep their existing
concurrent behavior rather than serializing behind a leader.

## Downstream Cacheability

Sidekick chooses response cache headers from **why** a request skipped the cache, not
merely that it did. "Sidekick declines to store this" and "nobody may store this" are
different statements, and treating them the same meant every bypassed response was
marked `no-store` — telling browsers and CloudFront to discard content they were
entitled to keep, so the same object was re-fetched from origin forever.

| Situation | Emitted |
|---|---|
| HIT / 304 | `public, max-age=<remaining ttl>` + `Age` |
| MISS | `public, max-age=<ttl>` + `Age: 0` — a MISS is as cacheable as a HIT, it just was not in the cache yet |
| Bypass: `nocache` prefixes, WordPress login cookie | `private, no-store` |
| Bypass: `nocache_regex`, `nocache_home`, uncacheable response | origin's own `Cache-Control`, untouched |
| Bypass: debug query | `no-cache, no-store, must-revalidate` |
| **Any** cookie-varied request | `private, no-store` — overrides everything above |

The `nocache` prefix list is treated as private because it names application areas
(`/wp-admin`, `/wp-json`, `/sitepro`) whose responses are user-specific.
`nocache_regex` is treated as policy because it names file types Sidekick chooses not
to store — ordinary public content the browser and CDN should still cache.

Responses the origin itself marks `no-store` or `private` are never cached by Sidekick
and are passed through as `private, no-store`.

Set `bypass_cache_control nostore` to restore the old blanket behavior on every
non-HIT path. It is an escape hatch, not a recommended setting; cookie-varied
responses stay private under both values.

## Shared Cache Safety

When a request carries a cookie matching `cache_key_cookies`, the cache key — and
therefore the response — is specific to that visitor's session. Sidekick emits such
responses as:

```
Cache-Control: private, no-store
Vary: Accept-Encoding, Cookie
```

This keeps them out of CloudFront, corporate proxies and any other shared cache, while
Sidekick's own cache still segments them correctly by key. Anonymous responses are
unaffected and remain `public`, so CDN hit rate for ordinary traffic is unchanged.

All downstream cache headers (`Cache-Control`, `Pragma`, `Age`, `Vary`) are written in
exactly one place, `applyDownstreamCacheHeaders`, from the same value that varied the
cache key. A unit test walks the package AST and fails the build if any other function
writes them, so adding a cookie to `cache_key_cookies` cannot silently make a
session-specific response shareable.

### Static Asset Exemption

WordPress scopes `wordpress_logged_in_<hash>` to `/`, so a logged-in visitor sends it
with **every** request — including stylesheets, scripts, fonts and images. Without an
exemption those all bypass the cache and hit the origin, even though their bytes are
identical to what an anonymous visitor receives.

Paths matching `static_asset_regex` are therefore exempt from both the login-cookie
bypass and cookie cache keying, so a single shared entry serves everyone.

The default pattern is deliberately conservative. It **excludes** `pdf`, `zip`,
archives, office documents and media (`mp4`, `mov`, `webm`, `mp3`), because those are
the formats membership and download-protection plugins gate behind a login check —
exempting them could share one visitor's gated copy with everyone. Widen the pattern
for a site only when you know it has no such plugin.

The `nocache` prefixes, `nocache_regex` and `nocache_home` are all evaluated **before**
this exemption, so an explicitly excluded path stays excluded even if it looks static.

## NoCache Path Matching

The `nocache` option uses **prefix matching** for paths:

```caddyfile
sidekick {
    # This will bypass cache for:
    # - /wp-admin
    # - /wp-admin/index.php
    # - /wp-admin/users.php
    # - /wp-json
    # - /wp-json/wp/v2/posts
    # - /wp-json-custom (any path starting with /wp-json)
    nocache /wp-admin /wp-json
}
```

This makes it easy to exclude entire sections of your site from caching without listing every possible path.

## Size Configuration Guidelines

### Memory vs Disk Trade-offs

| Setting | Use Case | Example Value |
|---------|----------|---------------|
| `cache_memory_item_max_size` | Small, frequently accessed pages | `4MB` |
| `cache_memory_max_size` | Available RAM for caching | `256MB` |
| `cache_memory_max_percent` | Percentage of RAM to use | `10` (10% of RAM) |
| `cache_memory_stream_to_disk_size` | Balance memory vs disk I/O | `5MB` |
| `cache_disk_item_max_size` | Prevent caching huge responses | `100MB` |
| `cache_disk_max_size` | Total disk space for cache | `10GB` |
| `cache_disk_max_percent` | Percentage of disk to use | `5` (5% of disk) |
| `cache_disk_max_count` | Max items on disk (LRU eviction) | `100000` |

### Special Values

- `0` = Feature disabled
- `-1` = Unlimited (use with caution)
- Human-readable byte-sizes: `1KB`, `10MB`, `1.5GB`

## Performance Tuning

### For Shared Hosting (Limited Resources)
```caddyfile
sidekick {
    cache_memory_max_size 64MB
    cache_memory_max_count 10000
    cache_memory_stream_to_disk_size 2MB
    cache_disk_item_max_size 20MB
    cache_disk_max_size 1GB
    cache_disk_max_count 10000  # Limited items for small disk
}
```

### For VPS/Dedicated Server (Abundant Resources)
```caddyfile
sidekick {
    cache_memory_max_percent 25     # Use 25% of RAM
    cache_memory_max_count -1       # Unlimited count
    cache_memory_stream_to_disk_size 20MB
    cache_disk_item_max_size 200MB
    cache_disk_max_percent 10       # Use 10% of disk space
    cache_disk_max_count -1          # Unlimited items on disk
}
```

## Monitoring

### Response Headers

Check cache headers in responses:
- `X-Sidekick-Cache: HIT` - Served from cache
- `X-Sidekick-Cache: MISS` - Not in cache, response cached
- `X-Sidekick-Cache: BYPASS` - Caching bypassed

### Prometheus Metrics

Sidekick automatically integrates with Caddy's metrics module to provide comprehensive cache monitoring. When metrics are enabled in your Caddyfile, Sidekick exposes detailed Prometheus metrics with zero additional configuration.

#### Available Metrics

**Cache Storage Metrics:**
- `caddy_sidekick_cache_used_bytes` - Current cache usage in bytes (labels: type=[memory|disk|total], server)
- `caddy_sidekick_cache_limit_bytes` - Cache size limit in bytes (labels: type=[memory|disk|total], server)
- `caddy_sidekick_cache_used_percent` - Cache usage as percentage of limit (labels: type=[memory|disk|total], server)

**Cache Count Metrics:**
- `caddy_sidekick_cache_used_count` - Number of cached items (labels: type=[memory|disk|total], server)
- `caddy_sidekick_cache_limit_count` - Item count limit (labels: type=[memory|disk|total], server)

**Cache Operations:**
- `caddy_sidekick_cache_operations_total` - Total operations counter (labels: operation=[get|bypass|store|purge], status=[hit|miss|success], server)
- `caddy_sidekick_cache_rate_percent` - Cache hit/miss/bypass rates as percentages (labels: type=[hit|miss|bypass], server)

**Performance Metrics:**
- `caddy_sidekick_response_time_ms` - Response time histogram in milliseconds (labels: cache_status=[hit|miss|bypass], server)
- `caddy_sidekick_cache_size_distribution_bytes` - Distribution of cached item sizes (labels: type=[memory|disk], server)

Special values: `0` = disabled, `-1` = unlimited, `>0` = actual limit

#### Enabling Metrics

Sidekick metrics can be enabled by adding the `metrics` option to your sidekick configuration.

**Important:** The admin API must be enabled in your Caddyfile for the metrics endpoints to work:

```caddyfile
{
    # Enable admin API (required for sidekick metrics endpoints)
    admin localhost:2019
}

example.com {
    sidekick {
        # Enable metrics collection
        metrics /metrics/sidekick
        
        # Other cache configuration
        cache_dir /var/www/cache
        # ... other settings ...
    }
}
```

When metrics are enabled, they are available in two locations:

1. **Admin API endpoint** (detailed sidekick-specific metrics):
   - URL: `http://localhost:2019/metrics/sidekick` (Prometheus format)
   - URL: `http://localhost:2019/metrics/sidekick/stats` (JSON format)
   - Note: The admin API listens on localhost:2019 by default. Replace with your configured admin address if different.

2. **Caddy's standard metrics endpoint** (includes sidekick metrics):
   ```caddyfile
   {
       servers {
           metrics  # Enable Caddy's metrics collection
       }
   }
   
   example.com {
       handle /metrics {
           metrics  # Expose metrics publicly
       }
   }
   ```
   - URL: `https://example.com/metrics` (all Caddy metrics including sidekick)

**Note:** If the `metrics` option is not specified in the sidekick configuration, metrics collection is disabled to save resources. The admin API endpoints will not be available, and sidekick metrics will not appear in Caddy's standard metrics endpoint.

#### Example Prometheus Queries

**Cache Hit Rate:**
```promql
sum(rate(caddy_sidekick_cache_operations_total{operation="get",status="hit"}[5m])) /
sum(rate(caddy_sidekick_cache_operations_total{operation="get"}[5m])) * 100
```

**Memory Usage Percentage:**
```promql
caddy_sidekick_cache_used_bytes{type="memory"} / 
caddy_sidekick_cache_limit_bytes{type="memory"} * 100
```

**Average Response Time by Cache Status:**
```promql
rate(caddy_sidekick_response_time_ms_sum[5m]) / 
rate(caddy_sidekick_response_time_ms_count[5m])
```

#### Monitoring Best Practices

1. **Key Metrics to Watch:**
   - Cache hit rate (target: >80%)
   - Memory usage (alert: >90%)
   - Disk usage (alert: >95%)
   - Response times (P95 <1s)

2. **Recommended Alerts:**
   - Low cache hit rate (<50%)
   - High memory/disk usage (>90%)
   - Slow response times (P95 >1s)
   - High error rates

3. **Prometheus Scrape Configuration:**
```yaml
scrape_configs:
  - job_name: 'caddy_sidekick'
    static_configs:
      - targets: ['localhost:2019']  # Replace with your admin port
    scheme: http  # Use https if admin uses TLS
    metrics_path: /metrics/sidekick
```

Performance impact is minimal (<1% CPU overhead, ~10KB per metric series).

#### Complete Example with Monitoring Stack

```yaml
version: '3.8'

services:
  caddy:
    image: caddy:latest
    build:
      context: .
      dockerfile: Dockerfile
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
      - "2019:2019"  # Admin API for metrics
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy_data:/data
      - caddy_config:/config
      - caddy_cache:/var/cache/sidekick
    environment:
      - SIDEKICK_CACHE_MEMORY_MAX_SIZE=256MB
      - SIDEKICK_CACHE_DISK_MAX_SIZE=10GB

  prometheus:
    image: prom/prometheus
    ports:
      - "9090:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'

  grafana:
    image: grafana/grafana
    ports:
      - "3000:3000"
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=admin
    depends_on:
      - prometheus

volumes:
  caddy_data:
  caddy_config:
  caddy_cache:
```

**Prometheus Configuration (prometheus.yml):**

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'caddy'
    static_configs:
      - targets: ['caddy:2019']  # Replace 2019 with your admin port
    metrics_path: /metrics
  
  - job_name: 'caddy_sidekick'
    static_configs:
      - targets: ['caddy:2019']  # Replace 2019 with your admin port
    metrics_path: /metrics/sidekick
```

**Note:** Metrics are only collected when Caddy's metrics module is enabled. If metrics are not enabled in Caddy, Sidekick will operate normally without collecting metrics.

#### Alerting Rules for Prometheus

Create an `alerts.yml` file:
```yaml
groups:
  - name: caddy_sidekick
    rules:
      - alert: LowCacheHitRate
        expr: caddy_sidekick_cache_rate_percent{type="hit"} < 50
        for: 5m
        annotations:
          summary: "Cache hit rate below 50%"
      
      - alert: HighMemoryUsage
        expr: caddy_sidekick_cache_used_percent{type="memory"} > 90
        for: 2m
        annotations:
          summary: "Memory cache >90% full"
      
      - alert: HighDiskUsage
        expr: caddy_sidekick_cache_used_percent{type="disk"} > 95
        for: 5m
        annotations:
          summary: "Disk cache >95% full"
```

## Troubleshooting

### Cache not working?
1. Check response headers for `X-Sidekick-Cache`
2. Verify paths aren't in `nocache` list
3. Ensure response codes are in `cache_response_codes`
4. Check WordPress login cookies aren't set

### High memory usage?
1. Reduce `cache_memory_max_size` or use `cache_memory_max_percent`
2. Lower `cache_memory_stream_to_disk_size`
3. Decrease `cache_memory_max_count`

### Disk space issues?
1. Reduce `cache_ttl`
2. Lower `cache_disk_item_max_size`
3. Set `cache_disk_max_size` or `cache_disk_max_percent`
4. Implement regular cache purging

## License

MIT License

## Acknowledgements

This project was originally inspired by FrankenWP and it's Sidekick drop-in, and is designed specifically for Caddy web server with PHP/WordPress optimization in mind.
