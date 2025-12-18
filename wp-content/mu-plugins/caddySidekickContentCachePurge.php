<?php
/**
 * Plugin Name:     Content Cache Purge
 * Author:          Stephen Miracle
 * Description:     Purge the content on publish.
 * Version:         0.1.0
 *
 */


 add_action("save_post", function ($id) {
    $link = get_permalink($id);
    $url = get_site_url() . $_SERVER["SIDEKICK_PURGE_URI"];
    wp_remote_post($url, [
        "body" => [
            "paths" => [wp_make_link_relative($link) . "/*"]
        ],
        "headers" => [
            $_SERVER["SIDEKICK_PURGE_HEADER"] => $_SERVER["SIDEKICK_PURGE_TOKEN"],
        ],
        "sslverify" => false,
    ]);
 });
