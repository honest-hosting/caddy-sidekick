<?php
/**
 * Plugin Name:     Caddy Sidekick Content Cache Purge
 * Author:          Stephen Miracle, Kyle Mott
 * Description:     Purge the content on publish.
 * Version:         0.1.1
 *
 */
if (!defined('ABSPATH')) {
    exit;
}

/**
* Retrieve the Sidekick configuration
*/
function sidekick_get_config() {
    static $conf = null;

    if ($conf !== null) {
        return $conf;
    }

    // prefer getenv() (server-level env)
    $path   = getenv('SIDEKICK_PURGE_PATH') ?: ($_SERVER['SIDEKICK_PURGE_PATH'] ?? null);
    $url    = getenv('SIDEKICK_PURGE_URL') ?: ($_SERVER['SIDEKICK_PURGE_URL'] ?? null);
    $header = getenv('SIDEKICK_PURGE_HEADER') ?: ($_SERVER['SIDEKICK_PURGE_HEADER'] ?? null);
    $token  = getenv('SIDEKICK_PURGE_TOKEN') ?: ($_SERVER['SIDEKICK_PURGE_TOKEN'] ?? null);

    if (!$path || !$header || !$token) {
        error_log("[Sidekick] Missing configuration: " . json_encode([
            "SIDEKICK_PURGE_PATH"   => (bool) $path,
            "SIDEKICK_PURGE_HEADER" => (bool) $header,
            "SIDEKICK_PURGE_TOKEN"  => (bool) $token,
        ]));
        $conf = null;
    } else {
        $conf = [
            'path'   => $path,
            'url'    => $url,
            'header' => $header,
            'token'  => $token,
        ];
    }

    return $conf;
}

/**
* Send purge request with an array of paths.
*/
function sidekick_purge_paths(array $paths) {
    $conf = sidekick_get_config();
    if (!$conf) {
        return;
    }

    // Use custom URL if provided, otherwise use WordPress site URL
    if (!empty($conf['url'])) {
        $url = $conf['url'] . $conf['path'];
    } else {
        $url = get_site_url() . $conf['path'];
    }

    $response = wp_remote_post($url, [
        'body' => ['paths' => array_values(array_unique($paths))],
        'headers' => [$conf['header'] => $conf['token']],
        'sslverify' => false,
        'timeout' => 3,
    ]);

    if (is_wp_error($response)) {
        error_log("[Sidekick] Purge failed: " . $response->get_error_message());
    } else {
        error_log("[Sidekick] Purged paths: " . json_encode($paths));
    }
}

/**
* Build a list of paths related to a post.
*/
function sidekick_purge_paths_for_post($post_id) {
    $post = get_post($post_id);
    if (!$post) {
        return [];
    }

    $paths = [];

    // Single post
    $link = wp_make_link_relative(get_permalink($post_id));
    $paths[] = $link . "/*";

    // Categories
    $cats = get_the_category($post_id);
    if ($cats) {
        foreach ($cats as $cat) {
            $paths[] = "/category/{$cat->slug}/*";
        }
    }

    // Tags
    $tags = get_the_tags($post_id);
    if ($tags) {
        foreach ($tags as $tag) {
            $paths[] = "/tag/{$tag->slug}/*";
        }
    }

    // Feeds
    $paths[] = "/feed/";
    $paths[] = $link . "/feed/";

    return array_unique($paths);
}

/**
* Purge on post saves.
*/
add_action('save_post', function ($post_id, $post, $update) {
    if ($post->post_type === 'revision') {
        return;
    }
    sidekick_purge_paths(sidekick_purge_paths_for_post($post_id));
}, 10, 3);

/**
* Purge on post status transitions.
*/
add_action('transition_post_status', function ($new, $old, $post) {
    if ($new !== $old) {
        sidekick_purge_paths(sidekick_purge_paths_for_post($post->ID));
    }
}, 10, 3);

/**
* Purge when comments are added.
*/
add_action('comment_post', function ($comment_id, $comment_approved) {
    $comment = get_comment($comment_id);
    if ($comment && $comment->comment_post_ID) {
        sidekick_purge_paths(sidekick_purge_paths_for_post($comment->comment_post_ID));
    }
}, 10, 2);

/**
* Purge when comments are updated.
*/
add_action('edit_comment', function ($comment_id) {
    $comment = get_comment($comment_id);
    if ($comment && $comment->comment_post_ID) {
        sidekick_purge_paths(sidekick_purge_paths_for_post($comment->comment_post_ID));
    }
});

/**
* Purge when comments are deleted.
*/
add_action('delete_comment', function ($comment_id) {
    $comment = get_comment($comment_id);
    if ($comment && $comment->comment_post_ID) {
        sidekick_purge_paths(sidekick_purge_paths_for_post($comment->comment_post_ID));
    }
});

/**
* Purge when menus change.
*/
add_action('wp_update_nav_menu', function () {
    // minimally purge homepage + feed
    sidekick_purge_paths([
        "/",
        "/feed/",
    ]);
});

/**
* Optional: taxonomy updates.
*/
add_action('edited_terms', function ($term_id, $taxonomy) {
    $term = get_term($term_id, $taxonomy);
    if ($term) {
        sidekick_purge_paths(["/{$taxonomy}/{$term->slug}/*"]);
    }
}, 10, 2);
