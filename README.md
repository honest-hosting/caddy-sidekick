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
| `SIDEKICK_PURGE_KEY` | Secret key for purge authentication | _(none)_ |
| `SIDEKICK_PURGE_PATH` | API endpoint for cache purging | `/__sidekick/purge` |
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

### Caddyfile Example

Complete example for a WordPress site:

```caddyfile
{
    # Global options
    order sidekick before file_server
    
    # Optional: admin endpoint
    admin localhost:2019
}

example.com {
    # Enable Sidekick caching
    sidekick {
        # Cache storage location
        cache_dir /var/cache/caddy/sidekick
        
        # Cache TTL in seconds (1 hour)
        cache_ttl 3600
        
        # HTTP status codes to cache
        cache_response_codes 200 404 301 302
        
        # Paths to bypass cache
        nocache /wp-admin /wp-json /wp-login.php
        
        # Don't cache home page (optional)
        nocache_home false
        
        # Regex for file types to bypass
        # Default includes common static assets
        # nocache_regex "\.(jpg|jpeg|png|gif|ico|css|js|svg|woff|woff2|ttf|eot|otf)$"
        
        # Purge endpoint configuration
        purge_path /__sidekick/purge
        purge_key "your-secret-purge-key-here"  # Update for your deployment
        
        # Memory cache limits
        cache_memory_item_max_size 4MB      # Max size for single item in memory
        cache_memory_max_size 128MB         # Total memory cache limit (or use cache_memory_max_percent)
        # cache_memory_max_percent 10       # Alternative: Use 10% of available RAM
        cache_memory_max_count 32768        # Max items in memory
        cache_memory_stream_to_disk_size 10MB # Stream to disk if larger than this
        
        # Disk cache limits
        cache_disk_item_max_size 100MB      # Max size for any cached item on disk
        cache_disk_max_size 10GB            # Total disk cache limit (or use cache_disk_max_percent)
        # cache_disk_max_percent 5          # Alternative: Use 5% of available disk space
        cache_disk_max_count 100000         # Max number of items on disk (LRU eviction)
        
        # Cache key customization
        cache_key_queries page sort filter  # Include these query params
        cache_key_headers Accept-Language   # Vary cache by these headers
        cache_key_cookies wordpress_logged_in_* # Include these cookies
    }
    
    # PHP handling with FrankenPHP or php_fastcgi
    php {
        root * /var/www/html
    }
    
    # Or use php_fastcgi
    # root * /var/www/html
    # php_fastcgi localhost:9000
    
    # Static file serving
    file_server
    
    # Compression
    encode gzip
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
                          "cache_dir": "/var/cache/caddy/sidekick",
                          "cache_ttl": 3600,
                          "cache_response_codes": ["200", "404", "301", "302"],
                          "nocache": ["/wp-admin", "/wp-json", "/wp-login.php"],
                          "nocache_home": false,
                          "nocache_regex": "\\.(jpg|jpeg|png|gif|ico|css|js|svg|woff|woff2|ttf|eot|otf)$",
                          "purge_path": "/__sidekick/purge",
                          "purge_key": "your-secret-purge-key-here",
                          "cache_memory_item_max_size": 4194304,
                          "cache_memory_max_size": 134217728,
                          "cache_memory_max_percent": 0,
                          "cache_memory_max_count": 32768,
                          "cache_memory_stream_to_disk_size": 10485760,
                          "cache_disk_item_max_size": 104857600,
                          "cache_disk_max_size": 10737418240,
                          "cache_disk_max_percent": 0,
                          "cache_disk_max_count": 100000,
                          "cache_key_queries": ["page", "sort", "filter"],
                          "cache_key_headers": ["Accept-Language"],
                          "cache_key_cookies": ["wordpress_logged_in_*"]
                        }
                      ]
                    },
                    {
                      "handle": [
                        {
                          "handler": "php",
                          "root": "/var/www/html"
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
          ]
        }
      }
    }
  }
}
```

## Usage Examples

### Basic WordPress Configuration

Minimal configuration for a standard WordPress site:

```caddyfile
example.com {
    sidekick {
        cache_dir /var/cache/sidekick
        purge_key "secret123"  # TODO: Update with secure key
    }
    
    root * /var/www/html
    php_fastcgi localhost:9000
    file_server
}
```

### High-Traffic Site Configuration

Optimized for high-traffic WordPress sites:

```caddyfile
example.com {
    sidekick {
        # Storage
        cache_dir /nvme/cache/sidekick  # Fast NVMe storage
        
        # Performance
        cache_ttl 7200                   # 2 hours
        cache_memory_max_size 512MB      # More memory for hot content
        cache_memory_max_count 100000    # More items in memory
        cache_memory_stream_to_disk_size 5MB # Stream earlier to save memory
        cache_disk_max_size 50GB         # Large disk cache for static content
        cache_disk_max_count 500000      # Allow many items on disk
        
        # Selective caching
        cache_response_codes 200 301 302
        nocache /wp-admin /wp-json /cart /checkout
        
        # Cache variations for e-commerce
        cache_key_queries "*"             # All query params matter
        cache_key_cookies wordpress_logged_in_* woocommerce_*
        
        # Security
        purge_key "complex-secure-key-here"  # TODO: Update
    }
    
    # ... rest of config
}
```

### Multi-Language Site Configuration

Configuration for sites with multiple languages:

```caddyfile
example.com {
    sidekick {
        cache_dir /var/cache/sidekick
        
        # Vary cache by language
        cache_key_headers Accept-Language Cookie
        cache_key_cookies pll_language qtrans_front_language
        
        # Don't cache language switcher
        nocache /wp-admin /wp-json /?lang= /language-switcher
        
        purge_key "secure-key"  # TODO: Update
    }
    
    # ... rest of config
}
```

## Cache Management

### Purging Cache

#### Purge all cache:
```bash
curl -X POST https://example.com/__sidekick/purge \
  -H "X-Sidekick-Purge-Key: your-secret-purge-key"
```

#### Purge specific path:
```bash
curl -X POST https://example.com/__sidekick/purge/blog/post-slug \
  -H "X-Sidekick-Purge-Key: your-secret-purge-key"
```

#### List cached items:
```bash
curl https://example.com/__sidekick/purge \
  -H "X-Sidekick-Purge-Key: your-secret-purge-key"
```

### WordPress Integration

Add to your theme's `functions.php` or a custom plugin:

```php
// Purge cache when posts are updated
add_action('save_post', function($post_id) {
    if (wp_is_post_revision($post_id)) {
        return;
    }
    
    $permalink = get_permalink($post_id);
    $path = parse_url($permalink, PHP_URL_PATH);
    
    wp_remote_post(home_url('/__sidekick/purge' . $path), [
        'headers' => [
            'X-Sidekick-Purge-Key' => 'your-secret-purge-key'
        ]
    ]);
});

// Purge all cache when theme is switched
add_action('switch_theme', function() {
    wp_remote_post(home_url('/__sidekick/purge'), [
        'headers' => [
            'X-Sidekick-Purge-Key' => 'your-secret-purge-key'
        ]
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

This project was originally inspired by FrankenWP and is designed specifically for Caddy web server with PHP/WordPress optimization in mind.
