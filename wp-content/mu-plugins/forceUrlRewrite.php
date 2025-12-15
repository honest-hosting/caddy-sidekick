<?php
/**
 * Plugin Name:     Force URL Rewrite
 * Author:          Stephen Miracle
 * Description:     Used to set the got_url_rewrite to true for FrankenPHP.
 * Version:         0.1.0
 *
 */


add_filter('got_url_rewrite', function() { return true; });
