# Caddy Sidekick

Lightning-fast server side caching Caddy module for PHP application(s), with an emphasis on WordPress

## Building

TBD

## Configuration

### Sidekick Configuration

- `CACHE_LOC`: Where to store cache. Defaults to /var/www/html/wp-content/cache
- `CACHE_RESPONSE_CODES`: Which status codes to cache. Defaults to 200,404,405
- `BYPASS_PATH_PREFIX`: Which path prefixes to not cache. Defaults to /wp-admin,/wp-json
- `BYPASS_HOME`: Whether to skip caching home. Defaults to false.
- `PURGE_KEY`: Create a purge key that must be validated on purge requests. Helps to prevent malicious intent. No default.
- `PURGE_PATH`: Create a custom route for the cache purge API path. Defaults to /\_\_sidekick/purge.
- `TTL`: Defines how long objects should be stored in cache. Defaults to 6000. Unit in seconds. 0 or negative value means cache forever.
- `CACHE_HEADER_NAME`: Change header name for the cache state check. Defaults to X-Sidekick-Cache.
- `CACHE_MEM_ITEM_SIZE`: if a response size larger then this value, it will not cache and just bypass. Unit in byte. Defaults to `4194304` (4 MB). this will affect temporary momory usage when try to caching response.
- `CACHE_MEM_ALL_SIZE`: limit how much memory should use for in-memory cache. Unit in byte. Defaults to `134217728` (128 MB). negative value means disable limit.
- `CACHE_MEM_ALL_COUNT`: limit how many item should keep in memory. Defaults to `32768` (32 k). negative value means disable limit.

## Usage

To use Sidekick in your Caddyfile:

```
{
    order sidekick before file_server
}

example.com {
    sidekick {
        loc /var/www/cache
        ttl 3600
        cache_response_codes 200,404
        bypass_path_prefixes /wp-admin,/wp-json
    }
    
    # Your PHP/WordPress configuration here
}
```

## Questions

### Why Sidekick?

Other (unnamed) open-source webservers have efficient caching for dynamic WordPress pages, but no such module exists for Caddy. Sidekick fills that gap, providing a fast, efficient caching layer for WordPress sites running on Caddy.

### Why FrankenPHP?

FrankenPHP is built on Caddy, a modern web server built in Go. It is secure & performs well when scaling becomes important. It also allows us to take advantage of built-in mature concurrency through goroutines into a single Docker image. high performance in a single lean image.

## Acknowledgements

This project was originally based off of the [FrankenWP](https://github.com/StephenMiracle/frankenwp) project, but is instead focused on a generic PHP static response caching module for use in Caddy (FrankenPHP).

Many thanks to the FrankenWP team for their pioneering work in this area.
