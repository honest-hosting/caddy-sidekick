#!/bin/bash
# Startup script for integration tests
# Runs PHP built-in server and Caddy together

set -e

echo "Starting integration test environment..."

# # Start PHP built-in server in the background on port 9000
# echo "Starting PHP server on port 9000..."
# php -S 127.0.0.1:9000 -t /var/www/html &
# PHP_PID=$!

# # Give PHP server time to start
# sleep 2

# # Check if PHP server is running
# if ! kill -0 $PHP_PID 2>/dev/null; then
#     echo "ERROR: PHP server failed to start"
#     exit 1
# fi

# echo "PHP server started with PID $PHP_PID"

# # Create a simple Caddyfile that proxies to PHP server
# cat > /tmp/Caddyfile <<'EOF'
# {
# 	order sidekick before file_server
# 	admin off
# 	log {
# 		level INFO
# 	}
# }

# :80 {
# 	# Enable sidekick caching with test configuration
# 	sidekick {
# 		cache_dir /var/cache/sidekick
# 		cache_ttl 60
# 		cache_response_codes 200 404 301 302
# 		nocache /wp-admin /wp-json /non-cacheable /api/realtime
# 		nocache_home false
# 		nocache_regex "\\.(mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe)$"
# 		purge_uri /__sidekick/purge
# 		purge_header X-Sidekick-Purge
# 		purge_token test-token-12345
# 		cache_memory_item_max_size 1MB
# 		cache_memory_max_size 64MB
# 		cache_memory_max_count 100
# 		cache_memory_stream_to_disk_size 512KB
# 		cache_disk_item_max_size 10MB
# 		cache_disk_max_size 256MB
# 		cache_disk_max_count 1000
# 		cache_key_queries name page size
# 		cache_key_headers Accept-Language X-Test-Header
# 		cache_key_cookies test_session wordpress_logged_in_*
# 	}

# 	root * /var/www/html

# 	# Proxy PHP files to PHP built-in server
# 	@phpFiles path *.php
# 	handle @phpFiles {
# 		reverse_proxy 127.0.0.1:9000 {
# 			header_up Host {http.request.host}
# 			header_up X-Real-IP {http.request.remote}
# 			header_up X-Forwarded-For {http.request.remote}
# 			header_up X-Forwarded-Proto {http.request.scheme}
# 		}
# 	}

# 	# Serve static files
# 	file_server

# 	# Add response headers for debugging
# 	header {
# 		X-Test-Server "Caddy-Sidekick-Integration"
# 	}

# 	# Handle errors
# 	handle_errors {
# 		respond "{http.error.status_code} {http.error.status_text}"
# 	}
# }
# EOF

echo "Starting Caddy server..."
if [ -f /var/www/etc/Caddyfile ]; then
    echo "Using provided Caddyfile from /var/www/etc/Caddyfile"
    exec /usr/local/bin/caddy run --config /var/www/etc/Caddyfile --adapter caddyfile
# else
#     echo "Using generated Caddyfile from /tmp/Caddyfile"
#     exec /usr/local/bin/caddy run --config /tmp/Caddyfile --adapter caddyfile
fi

# Cleanup on exit (this won't be reached with exec, but kept for completeness)
# trap "kill $PHP_PID 2>/dev/null" EXIT
