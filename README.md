# Caddy Sidekick

Lightning-fast server side caching Caddy module for PHP application(s), with an emphasis on WordPress

## Building

TBD

## Configuration

### Sidekick Configuration

#### Basic Cache Settings
- `CACHE_LOC`: Where to store cache. Defaults to /var/www/html/wp-content/cache
- `CACHE_RESPONSE_CODES`: Which status codes to cache. Defaults to 200,404,405
- `BYPASS_PATH_PREFIX`: Which path prefixes to not cache. Defaults to /wp-admin,/wp-json
- `BYPASS_HOME`: Whether to skip caching home. Defaults to false.
- `PURGE_KEY`: Create a purge key that must be validated on purge requests. Helps to prevent malicious intent. No default.
- `PURGE_PATH`: Create a custom route for the cache purge API path. Defaults to /\_\_sidekick/purge.
- `TTL`: Defines how long objects should be stored in cache. Defaults to 6000. Unit in seconds. 0 or negative value means cache forever.
- `CACHE_HEADER_NAME`: Change header name for the cache state check. Defaults to X-Sidekick-Cache.

#### Memory & Size Limits
- `CACHE_MEM_ITEM_SIZE`: Maximum size for a single item in memory cache. Unit in bytes. Defaults to `4194304` (4 MB).
- `CACHE_MEM_ALL_SIZE`: Total memory limit for in-memory cache. Unit in bytes. Defaults to `134217728` (128 MB). Negative value means no limit.
- `CACHE_MEM_ALL_COUNT`: Maximum number of items to keep in memory. Defaults to `32768` (32k). Negative value means no limit.
- `MAX_CACHEABLE_SIZE`: Maximum size for a response to be cached at all. Unit in bytes. Defaults to `104857600` (100 MB). Responses larger than this are never cached.
- `STREAM_TO_DISK_SIZE`: Size threshold for streaming responses directly to disk instead of memory buffering. Unit in bytes. Defaults to `10485760` (10 MB).

#### Cache Key Configuration
- `CACHE_KEY_HEADERS`: Comma-separated list of headers to include in cache key generation. Example: `Accept-Language,User-Agent`
- `CACHE_KEY_QUERIES`: Comma-separated list of query parameters to include in cache key. Use `*` to include all. Example: `page,sort,filter`
- `CACHE_KEY_COOKIES`: Comma-separated list of cookies to include in cache key. Example: `session_id,preferences`

## Usage

To use Sidekick in your Caddyfile:

```
{
    order sidekick before file_server
}

example.com {
    sidekick {
        # Basic configuration
        loc /var/www/cache
        ttl 3600
        cache_response_codes 200,404
        bypass_path_prefixes /wp-admin,/wp-json
        
        # Size limits
        max_cacheable_size 104857600    # 100MB max cacheable size
        stream_to_disk_size 10485760    # Stream to disk if > 10MB
        memory_item_max_size 4194304    # 4MB max per item in memory
        
        # Cache key customization
        cache_key_queries page,sort      # Include these query params in cache key
        cache_key_headers Accept-Language # Vary cache by language
        
        # Purge configuration
        purge_key your-secret-key-here
        purge_path /__sidekick/purge
    }
    
    # Your PHP/WordPress configuration here
}
```

## Features

### Performance Optimizations

- **Buffer Pooling**: Uses `sync.Pool` to efficiently reuse buffers for response handling, reducing memory allocations
- **Automatic Compression**: Automatically stores compressed versions (gzip, brotli, zstd) of cached responses when beneficial
- **Streaming to Disk**: Large responses are streamed directly to disk instead of buffering in memory, configurable via `stream_to_disk_size`
- **304 Not Modified Support**: Handles conditional requests with ETag and Last-Modified headers for bandwidth efficiency
- **Pre-compiled Regex**: Path bypass patterns are compiled once during initialization for better performance

### Advanced Cache Key Generation

The cache key can be customized to include:
- **Query Parameters**: Include specific or all query parameters in the cache key
- **Request Headers**: Vary cache based on headers like Accept-Language or User-Agent
- **Cookies**: Include specific cookie values for personalized caching

This allows for fine-grained control over cache variations, essential for dynamic WordPress sites.

### Size Management

- **Maximum Cacheable Size**: Responses larger than `max_cacheable_size` are never cached
- **Memory vs Disk Threshold**: Responses larger than `stream_to_disk_size` are streamed directly to disk
- **Per-Item Memory Limit**: Individual items in memory cache are limited by `memory_item_max_size`

## Questions

### Why Sidekick?

Other (unnamed) open-source webservers have efficient caching for dynamic WordPress pages, but no such module exists for Caddy. Sidekick fills that gap, providing a fast, efficient caching layer for WordPress sites running on Caddy.

### Why FrankenPHP?

FrankenPHP is built on Caddy, a modern web server built in Go. It is secure & performs well when scaling becomes important. It also allows us to take advantage of built-in mature concurrency through goroutines into a single Docker image. high performance in a single lean image.

## Acknowledgements

This project was originally based off of the [FrankenWP](https://github.com/StephenMiracle/frankenwp) project, but is instead focused on a generic PHP static response caching module for use in Caddy (FrankenPHP).

Many thanks to the FrankenWP team for their pioneering work in this area.
