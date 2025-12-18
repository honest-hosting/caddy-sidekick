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
| `SIDEKICK_CACHE_RESPONSE_CODES` | HTTP status codes to cache (comma-separated) | `200,404,405` |
| `SIDEKICK_NOCACHE` | Path prefixes to bypass cache (comma-separated) | `/wp-admin,/wp-json` |
| `SIDEKICK_NOCACHE_HOME` | Skip caching home page | `false` |
| `SIDEKICK_NOCACHE_REGEX` | Regex pattern for paths to bypass | `\.(jpg\|jpeg\|png\|gif\|ico\|css\|js\|svg\|woff\|woff2\|ttf\|eot\|otf\|mp4\|webm\|mp3\|ogg\|wav\|pdf\|zip\|tar\|gz\|7z\|exe\|doc\|docx\|xls\|xlsx\|ppt\|pptx)$` |
| `SIDEKICK_CACHE_TTL` | Cache time-to-live in seconds | `6000` |
| `SIDEKICK_PURGE_HEADER` | HTTP header name for purge token | `X-Sidekick-Purge` |
| `SIDEKICK_PURGE_URI` | API endpoint for cache purging (absolute path, only a-z0-9-_/ allowed) | `/__sidekick/purge` |
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
| `SIDEKICK_CACHE_KEY_HEADERS` | Headers to include in cache key (comma-separated) | _(none)_ |
| `SIDEKICK_CACHE_KEY_QUERIES` | Query parameters to include in cache key (comma-separated, use `*` for all) | _(none)_ |
| `SIDEKICK_CACHE_KEY_COOKIES` | Cookies to include in cache key (comma-separated) | _(none)_ |
| `SIDEKICK_WP_MU_PLUGIN_ENABLED` | Enable automatic WordPress mu-plugin management | `true` |
| `SIDEKICK_WP_MU_PLUGIN_DIR` | Directory for WordPress mu-plugins | `/var/www/html/wp-content/mu-plugins` |

**Note:** When either memory or disk cache is enabled, all purge-related options (`SIDEKICK_PURGE_HEADER`, `SIDEKICK_PURGE_URI`, `SIDEKICK_PURGE_TOKEN`) are required to be set.

### Quick Start

Minimal configuration for a WordPress site:

```caddyfile
{
    order sidekick before rewrite
}

example.com {
    sidekick {
        cache_dir /var/www/cache
        cache_ttl 3600
        
        purge_uri /__sidekick/purge
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
    admin off
    
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
        # Cache storage location
        cache_dir /var/www/cache
        
        # Cache TTL in seconds (1 hour)
        cache_ttl 3600
        
        # HTTP status codes to cache
        cache_response_codes 200 301 302
        
        # Paths to bypass cache (WordPress paths)
        nocache /wp-admin /wp-json /wp-login.php
        
        # Don't cache home page (optional)
        nocache_home false
        
        # Regex for file types to bypass
        # Exclude large media files from cache
        nocache_regex "\\.(mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe)$"
        
        # Purge endpoint configuration (required when cache is enabled)
        purge_uri /__sidekick/purge
        purge_header X-Sidekick-Purge
        purge_token "your-secret-token-here"  # CHANGE THIS!
        
        # Memory cache limits
        cache_memory_item_max_size 4MB
        cache_memory_max_size 128MB
        cache_memory_max_count 32768
        cache_memory_stream_to_disk_size 10MB
        
        # Disk cache limits
        cache_disk_item_max_size 100MB
        cache_disk_max_size 10GB
        cache_disk_max_count 100000
        
        # Cache key customization
        cache_key_queries page sort filter     # Include these query params
        cache_key_headers Accept-Language      # Vary cache by these headers
        cache_key_cookies wordpress_logged_in_* # Include these cookies
        
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

For those preferring JSON configuration:

```json
{
  "apps": {
    "http": {
      "servers": {
        "srv0": {
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
                          "cache_dir": "/var/www/cache",
                          "cache_ttl": 3600,
                          "cache_response_codes": ["200", "301", "302"],
                          "nocache": ["/wp-admin", "/wp-json", "/wp-login.php"],
                          "nocache_home": false,
                          "nocache_regex": "\\.(mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe)$",
                          "purge_uri": "/__sidekick/purge",
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

### Manual WordPress Integration

If you prefer manual integration, add to your theme's `functions.php` or a custom plugin:

```php
// Purge cache when posts are updated
add_action('save_post', function($post_id) {
    if (wp_is_post_revision($post_id)) {
        return;
    }
    
    $permalink = get_permalink($post_id);
    $path = parse_url($permalink, PHP_URL_PATH);
    
    wp_remote_post(home_url('/__sidekick/purge'), [
        'headers' => [
            'X-Sidekick-Purge' => 'your-secret-token',
            'Content-Type' => 'application/json'
        ],
        'body' => json_encode(['paths' => [$path]])
    ]);
});

// Purge all cache when theme is switched
add_action('switch_theme', function() {
    wp_remote_post(home_url('/__sidekick/purge'), [
        'headers' => [
            'X-Sidekick-Purge' => 'your-secret-token'
        ],
        'body' => '' // Empty body purges all
    ]);
});
```

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
- Human-readable sizes: `1KB`, `10MB`, `1.5GB`

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

Check cache headers in responses:
- `X-Sidekick-Cache: HIT` - Served from cache
- `X-Sidekick-Cache: MISS` - Not in cache, response cached
- `X-Sidekick-Cache: BYPASS` - Caching bypassed

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
