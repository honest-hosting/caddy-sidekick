<?php
/**
 * Integration Test PHP Server
 * Provides various endpoints for testing Caddy Sidekick caching functionality
 */

// Set default timezone
date_default_timezone_set('UTC');

// Get request path and method
$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
$method = $_SERVER['REQUEST_METHOD'];
$query = $_GET;

// Helper function to send JSON response
function jsonResponse($data, $statusCode = 200, $headers = []) {
    http_response_code($statusCode);
    header('Content-Type: application/json');
    foreach ($headers as $header => $value) {
        header("$header: $value");
    }
    echo json_encode($data, JSON_PRETTY_PRINT);
    exit;
}

// Helper function to send HTML response
function htmlResponse($content, $statusCode = 200, $headers = []) {
    http_response_code($statusCode);
    header('Content-Type: text/html; charset=utf-8');
    foreach ($headers as $header => $value) {
        header("$header: $value");
    }
    echo $content;
    exit;
}

// Helper function to send plain text response
function textResponse($content, $statusCode = 200, $headers = []) {
    http_response_code($statusCode);
    header('Content-Type: text/plain; charset=utf-8');
    foreach ($headers as $header => $value) {
        header("$header: $value");
    }
    echo $content;
    exit;
}

// Route handling
switch ($path) {
    case '/':
    case '/index.php':
        // Home page - cacheable by default
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Sidekick Integration Test</title>
</head>
<body>
    <h1>Caddy Sidekick Integration Test</h1>
    <p>Timestamp: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Request ID: ' . uniqid() . '</p>
    <ul>
        <li><a href="/cacheable">Cacheable Content</a></li>
        <li><a href="/non-cacheable">Non-Cacheable Content</a></li>
        <li><a href="/api/data">API Endpoint</a></li>
        <li><a href="/static/page">Static-like Page</a></li>
        <li><a href="/dynamic">Dynamic Content</a></li>
    </ul>
</body>
</html>');
        break;

    case '/health.php':
        // Health check endpoint
        textResponse('OK', 200);
        break;

    case '/cacheable':
        // Explicitly cacheable content with cache control headers
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Cacheable Content</title>
</head>
<body>
    <h1>This content is cacheable</h1>
    <p>Generated at: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Unique ID: ' . uniqid() . '</p>
    <p>This page should be cached and subsequent requests should show the same timestamp and ID.</p>
</body>
</html>', 200, [
            'Cache-Control' => 'public, max-age=3600',
            'X-Test-Type' => 'cacheable'
        ]);
        break;

    case '/non-cacheable':
        // Non-cacheable content with no-cache headers
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Non-Cacheable Content</title>
</head>
<body>
    <h1>This content is NOT cacheable</h1>
    <p>Generated at: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Unique ID: ' . uniqid() . '</p>
    <p>This page should never be cached. Each request should show a different timestamp and ID.</p>
</body>
</html>', 200, [
            'Cache-Control' => 'no-cache, no-store, must-revalidate',
            'Pragma' => 'no-cache',
            'Expires' => '0',
            'X-Test-Type' => 'non-cacheable'
        ]);
        break;

    case '/api/data':
        // JSON API endpoint - cacheable
        jsonResponse([
            'timestamp' => time(),
            'date' => date('Y-m-d H:i:s.u'),
            'data' => [
                'id' => uniqid(),
                'value' => rand(1, 1000),
                'message' => 'This is cacheable API data'
            ]
        ], 200, [
            'Cache-Control' => 'public, max-age=300',
            'X-Test-Type' => 'api-cacheable'
        ]);
        break;

    case '/api/realtime':
        // JSON API endpoint - non-cacheable
        jsonResponse([
            'timestamp' => microtime(true),
            'date' => date('Y-m-d H:i:s.u'),
            'realtime' => [
                'id' => uniqid(),
                'random' => rand(1, 1000000),
                'message' => 'This is real-time data, should not be cached'
            ]
        ], 200, [
            'Cache-Control' => 'no-cache, no-store',
            'X-Test-Type' => 'api-non-cacheable'
        ]);
        break;

    case '/static/page':
        // Static-like content that should be cached
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Static Page</title>
</head>
<body>
    <h1>Static Content Page</h1>
    <p>This page simulates static content that rarely changes.</p>
    <p>Generated: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Version: 1.0.0</p>
</body>
</html>', 200, [
            'Cache-Control' => 'public, max-age=7200',
            'ETag' => '"static-v1"',
            'X-Test-Type' => 'static'
        ]);
        break;

    case '/dynamic':
        // Dynamic content based on query parameters
        $name = isset($query['name']) ? htmlspecialchars($query['name']) : 'Guest';
        $page = isset($query['page']) ? intval($query['page']) : 1;

        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Dynamic Content</title>
</head>
<body>
    <h1>Hello, ' . $name . '!</h1>
    <p>You are viewing page ' . $page . '</p>
    <p>Generated at: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Session: ' . uniqid() . '</p>
</body>
</html>', 200, [
            'Cache-Control' => 'public, max-age=600',
            'Vary' => 'Accept-Encoding',
            'X-Test-Type' => 'dynamic',
            'X-Query-Name' => $name,
            'X-Query-Page' => $page
        ]);
        break;

    case '/error/404':
        // 404 error page - should be cacheable
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>404 Not Found</title>
</head>
<body>
    <h1>404 - Page Not Found</h1>
    <p>The page you requested does not exist.</p>
    <p>Error generated at: ' . date('Y-m-d H:i:s.u') . '</p>
</body>
</html>', 404, [
            'Cache-Control' => 'public, max-age=300',
            'X-Test-Type' => 'error-404'
        ]);
        break;

    case '/error/500':
        // 500 error - should not be cached
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>500 Internal Server Error</title>
</head>
<body>
    <h1>500 - Internal Server Error</h1>
    <p>Something went wrong on the server.</p>
    <p>Error ID: ' . uniqid() . '</p>
    <p>Time: ' . date('Y-m-d H:i:s.u') . '</p>
</body>
</html>', 500, [
            'Cache-Control' => 'no-cache, no-store',
            'X-Test-Type' => 'error-500'
        ]);
        break;

    case '/large':
        // Large response for testing streaming to disk
        $size = isset($query['size']) ? intval($query['size']) : 1024 * 1024; // Default 1MB
        $content = str_repeat('X', $size);
        textResponse($content, 200, [
            'Cache-Control' => 'public, max-age=3600',
            'X-Test-Type' => 'large-response',
            'X-Content-Size' => $size
        ]);
        break;

    case '/conditional':
        // Support for conditional requests (ETag/Last-Modified)
        $etag = '"resource-v1-' . date('Ymd') . '"';
        $lastModified = gmdate('D, d M Y 00:00:00') . ' GMT';

        // Check If-None-Match
        if (isset($_SERVER['HTTP_IF_NONE_MATCH']) && $_SERVER['HTTP_IF_NONE_MATCH'] === $etag) {
            http_response_code(304);
            header('ETag: ' . $etag);
            header('Cache-Control: public, max-age=3600');
            exit;
        }

        // Check If-Modified-Since
        if (isset($_SERVER['HTTP_IF_MODIFIED_SINCE']) && $_SERVER['HTTP_IF_MODIFIED_SINCE'] === $lastModified) {
            http_response_code(304);
            header('Last-Modified: ' . $lastModified);
            header('Cache-Control: public, max-age=3600');
            exit;
        }

        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>Conditional Response</title>
</head>
<body>
    <h1>Conditional Response Test</h1>
    <p>This page supports conditional requests.</p>
    <p>ETag: ' . $etag . '</p>
    <p>Last-Modified: ' . $lastModified . '</p>
</body>
</html>', 200, [
            'ETag' => $etag,
            'Last-Modified' => $lastModified,
            'Cache-Control' => 'public, max-age=3600',
            'X-Test-Type' => 'conditional'
        ]);
        break;

    // Test paths for wildcard patterns
    case '/path/image1.png':
    case '/path/image2.png':
    case '/path/image123.png':
        textResponse('Image content for ' . $path, 200, [
            'Content-Type' => 'image/png',
            'Cache-Control' => 'public, max-age=7200',
            'X-Test-Type' => 'image',
            'X-Test-Path' => $path
        ]);
        break;

    case '/path/subdir/image1.png':
    case '/path/subdir/image2.png':
        textResponse('Subdir image content for ' . $path, 200, [
            'Content-Type' => 'image/png',
            'Cache-Control' => 'public, max-age=7200',
            'X-Test-Type' => 'subdir-image',
            'X-Test-Path' => $path
        ]);
        break;

    // WordPress-like paths for nocache testing
    case '/wp-admin/index.php':
    case '/wp-admin/post.php':
        htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>WP Admin</title>
</head>
<body>
    <h1>WordPress Admin Area</h1>
    <p>This should never be cached (nocache path).</p>
    <p>Time: ' . date('Y-m-d H:i:s.u') . '</p>
</body>
</html>', 200, [
            'Cache-Control' => 'no-cache, no-store',
            'X-Test-Type' => 'wp-admin'
        ]);
        break;

    case '/wp-json/wp/v2/posts':
        jsonResponse([
            'posts' => [
                ['id' => 1, 'title' => 'Post 1', 'time' => microtime(true)],
                ['id' => 2, 'title' => 'Post 2', 'time' => microtime(true)]
            ],
            'generated' => date('Y-m-d H:i:s.u')
        ], 200, [
            'Cache-Control' => 'no-cache',
            'X-Test-Type' => 'wp-json'
        ]);
        break;

    case '/test-generate-large-file-mb':
        // Generate a large HTML response based on size parameter
        $sizeMB = isset($query['size']) ? floatval($query['size']) : 1.0;
        $sizeBytes = intval($sizeMB * 1000 * 1000); // Use decimal MB (1MB = 1,000,000 bytes)

        // Generate HTML content with repeated lorem ipsum text
        $loremIpsum = "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia deserunt mollit anim id est laborum. ";

        // Start with valid HTML structure
        $html = '<!DOCTYPE html>
<html>
<head>
    <title>Large File Test - ' . $sizeMB . 'MB</title>
    <meta charset="utf-8">
</head>
<body>
    <h1>Large File Test</h1>
    <p>This is a test file of approximately ' . $sizeMB . 'MB (' . number_format($sizeBytes) . ' bytes)</p>
    <p>Generated at: ' . date('Y-m-d H:i:s.u') . '</p>
    <p>Unique ID: ' . uniqid() . '</p>
    <hr>
    <div id="content">
';

        // Calculate how much padding we need
        $currentSize = strlen($html);
        $remainingSize = $sizeBytes - $currentSize - 100; // Leave space for closing tags

        // Add repeated lorem ipsum paragraphs until we reach the desired size
        $paragraph = '<p>' . $loremIpsum . '</p>' . PHP_EOL;
        $paragraphSize = strlen($paragraph);
        $paragraphsNeeded = intval($remainingSize / $paragraphSize);

        for ($i = 0; $i < $paragraphsNeeded; $i++) {
            $html .= $paragraph;
        }

        // Add any remaining bytes as a final paragraph with padding
        $currentSize = strlen($html);
        $finalPadding = $sizeBytes - $currentSize - 20; // Account for closing tags
        if ($finalPadding > 0) {
            $html .= '<p>' . str_repeat('X', $finalPadding) . '</p>' . PHP_EOL;
        }

        // Close HTML tags
        $html .= '
    </div>
</body>
</html>';

        // Send response with caching headers
        htmlResponse($html, 200, [
            'Cache-Control' => 'public, max-age=3600',
            'X-Test-Type' => 'large-file',
            'X-Test-Size-MB' => $sizeMB,
            'X-Test-Size-Bytes' => strlen($html)
        ]);
        break;

    default:
        // Handle paths starting with /path/ for testing wildcard purging
        if (strpos($path, '/path/') === 0) {
            textResponse('Dynamic path content: ' . $path . ' at ' . date('Y-m-d H:i:s.u'), 200, [
                'Cache-Control' => 'public, max-age=600',
                'X-Test-Type' => 'dynamic-path',
                'X-Test-Path' => $path
            ]);
        } else {
            // Default 404 for unknown paths
            htmlResponse('<!DOCTYPE html>
<html>
<head>
    <title>404 Not Found</title>
</head>
<body>
    <h1>404 - Not Found</h1>
    <p>Path not found: ' . htmlspecialchars($path) . '</p>
</body>
</html>', 404);
        }
        break;
}
