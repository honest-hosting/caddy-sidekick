package sidekick

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func TestMuPluginConfiguration(t *testing.T) {
	tests := []struct {
		name            string
		envVars         map[string]string
		expectedEnabled bool
		expectedDir     string
	}{
		{
			name:            "default values",
			envVars:         map[string]string{},
			expectedEnabled: true,
			expectedDir:     "/var/www/html/wp-content/mu-plugins",
		},
		{
			name: "disabled via env",
			envVars: map[string]string{
				"SIDEKICK_WP_MU_PLUGIN_ENABLED": "false",
			},
			expectedEnabled: false,
			expectedDir:     "/var/www/html/wp-content/mu-plugins",
		},
		{
			name: "custom directory via env",
			envVars: map[string]string{
				"SIDEKICK_WP_MU_PLUGIN_DIR": "/custom/path/mu-plugins",
			},
			expectedEnabled: true,
			expectedDir:     "/custom/path/mu-plugins",
		},
		{
			name: "both custom values",
			envVars: map[string]string{
				"SIDEKICK_WP_MU_PLUGIN_ENABLED": "true",
				"SIDEKICK_WP_MU_PLUGIN_DIR":     "/another/path",
			},
			expectedEnabled: true,
			expectedDir:     "/another/path",
		},
		{
			name: "invalid boolean value defaults to true",
			envVars: map[string]string{
				"SIDEKICK_WP_MU_PLUGIN_ENABLED": "invalid",
			},
			expectedEnabled: true,
			expectedDir:     "/var/www/html/wp-content/mu-plugins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up environment after test
			defer func() {
				_ = os.Unsetenv("SIDEKICK_WP_MU_PLUGIN_ENABLED")
				_ = os.Unsetenv("SIDEKICK_WP_MU_PLUGIN_DIR")
				_ = os.Unsetenv("SIDEKICK_CACHE_MEMORY_MAX_SIZE")
				_ = os.Unsetenv("SIDEKICK_CACHE_MEMORY_MAX_COUNT")
				_ = os.Unsetenv("SIDEKICK_CACHE_DIR")
			}()

			// Set environment variables
			for k, v := range tt.envVars {
				if err := os.Setenv(k, v); err != nil {
					t.Fatal(err)
				}
			}

			// Set minimal cache configuration to avoid purge validation errors
			if err := os.Setenv("SIDEKICK_CACHE_MEMORY_MAX_SIZE", "1MB"); err != nil {
				t.Fatal(err)
			}
			if err := os.Setenv("SIDEKICK_CACHE_MEMORY_MAX_COUNT", "100"); err != nil {
				t.Fatal(err)
			}

			// Use temp directory for cache to avoid permission issues
			tempDir := t.TempDir()
			if err := os.Setenv("SIDEKICK_CACHE_DIR", tempDir); err != nil {
				t.Fatal(err)
			}

			// Create sidekick instance with explicit defaults
			// (since we're not going through UnmarshalCaddyfile)
			s := &Sidekick{
				WPMuPluginEnabled: DefaultWPMuPluginEnabled,
				WPMuPluginDir:     DefaultWPMuPluginDir,
			}

			// Create mock context with logger
			ctx := caddy.Context{}

			// Provision (this will load env vars and set defaults)
			err := s.Provision(ctx)
			if err != nil {
				// For this test, we expect provision to succeed or fail due to mu-plugin issues only
				// The actual mu-plugin deployment will fail in tests since the directories don't exist
				// We ignore the error as it's expected in test environment
				_ = err
			}

			// Check configuration values
			if s.WPMuPluginEnabled != tt.expectedEnabled {
				t.Errorf("WPMuPluginEnabled = %v, want %v", s.WPMuPluginEnabled, tt.expectedEnabled)
			}
			if s.WPMuPluginDir != tt.expectedDir {
				t.Errorf("WPMuPluginDir = %q, want %q", s.WPMuPluginDir, tt.expectedDir)
			}
		})
	}
}

func TestMuPluginDeployment(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()
	wpContentDir := filepath.Join(tempDir, "wp-content")
	muPluginsDir := filepath.Join(wpContentDir, "mu-plugins")

	// Create wp-content directory (parent)
	if err := os.MkdirAll(wpContentDir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		enabled         bool
		createDirBefore bool
		expectFiles     bool
		expectDirCreate bool
	}{
		{
			name:            "enabled with existing directory",
			enabled:         true,
			createDirBefore: true,
			expectFiles:     true,
			expectDirCreate: false,
		},
		{
			name:            "enabled without existing directory",
			enabled:         true,
			createDirBefore: false,
			expectFiles:     true,
			expectDirCreate: true,
		},
		{
			name:            "disabled with existing directory",
			enabled:         false,
			createDirBefore: true,
			expectFiles:     false,
			expectDirCreate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up mu-plugins directory between tests
			if err := os.RemoveAll(muPluginsDir); err != nil {
				t.Fatal(err)
			}

			if tt.createDirBefore {
				if err := os.MkdirAll(muPluginsDir, 0755); err != nil {
					t.Fatal(err)
				}
			}

			// Create sidekick instance
			s := &Sidekick{
				WPMuPluginEnabled: tt.enabled,
				WPMuPluginDir:     muPluginsDir,
				logger:            zap.NewNop(),
			}

			// Run mu-plugin management
			err := s.manageMuPlugins()
			if err != nil {
				t.Errorf("manageMuPlugins() error = %v", err)
			}

			// Check if directory exists
			_, err = os.Stat(muPluginsDir)
			dirExists := !os.IsNotExist(err)

			if tt.expectDirCreate && !tt.createDirBefore && !dirExists {
				t.Error("Expected mu-plugins directory to be created")
			}

			// Check if files exist
			if tt.expectFiles {
				expectedFiles := []string{
					"caddySidekickContentCachePurge.php",
					"caddySidekickForceUrlRewrite.php",
				}

				for _, file := range expectedFiles {
					filePath := filepath.Join(muPluginsDir, file)
					if _, err := os.Stat(filePath); os.IsNotExist(err) {
						t.Errorf("Expected file %s to exist", file)
					}
				}
			}
		})
	}
}

func TestMuPluginRemoval(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()
	wpContentDir := filepath.Join(tempDir, "wp-content")
	muPluginsDir := filepath.Join(wpContentDir, "mu-plugins")

	// Create directories
	if err := os.MkdirAll(muPluginsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// First, deploy the files with enabled=true
	s := &Sidekick{
		WPMuPluginEnabled: true,
		WPMuPluginDir:     muPluginsDir,
		logger:            zap.NewNop(),
	}

	if err := s.manageMuPlugins(); err != nil {
		t.Fatal(err)
	}

	// Verify files exist
	expectedFiles := []string{
		"caddySidekickContentCachePurge.php",
		"caddySidekickForceUrlRewrite.php",
	}

	for _, file := range expectedFiles {
		filePath := filepath.Join(muPluginsDir, file)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Fatalf("File %s should exist after deployment", file)
		}
	}

	// Now disable and run again - files should be removed
	s.WPMuPluginEnabled = false
	if err := s.manageMuPlugins(); err != nil {
		t.Fatal(err)
	}

	// Verify files are removed
	for _, file := range expectedFiles {
		filePath := filepath.Join(muPluginsDir, file)
		if _, err := os.Stat(filePath); !os.IsNotExist(err) {
			t.Errorf("File %s should have been removed when disabled", file)
		}
	}
}

func TestMuPluginChecksumValidation(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()
	wpContentDir := filepath.Join(tempDir, "wp-content")
	muPluginsDir := filepath.Join(wpContentDir, "mu-plugins")

	// Create directories
	if err := os.MkdirAll(muPluginsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create sidekick instance
	s := &Sidekick{
		WPMuPluginEnabled: true,
		WPMuPluginDir:     muPluginsDir,
		logger:            zap.NewNop(),
	}

	// Deploy files initially
	if err := s.manageMuPlugins(); err != nil {
		t.Fatal(err)
	}

	// Modify one of the files
	testFile := filepath.Join(muPluginsDir, "caddySidekickContentCachePurge.php")
	originalContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}

	// Write modified content
	modifiedContent := append(originalContent, []byte("\n// Modified by test")...)
	if err := os.WriteFile(testFile, modifiedContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Run manageMuPlugins again - it should detect the change and restore the file
	if err := s.manageMuPlugins(); err != nil {
		t.Fatal(err)
	}

	// Read the file again and verify it's been restored
	restoredContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}

	if string(restoredContent) == string(modifiedContent) {
		t.Error("File should have been restored to original content")
	}

	// The restored content should match the original
	if string(restoredContent) != string(originalContent) {
		t.Error("Restored content doesn't match original embedded content")
	}
}

func TestMuPluginParentDirectoryCheck(t *testing.T) {
	// Use a path where the parent directory doesn't exist
	nonExistentPath := "/this/does/not/exist/mu-plugins"

	// Create sidekick instance with non-existent parent
	s := &Sidekick{
		WPMuPluginEnabled: true,
		WPMuPluginDir:     nonExistentPath,
		logger:            zaptest.NewLogger(t),
	}

	// This should not return an error (non-fatal) but should log an error
	err := s.manageMuPlugins()
	if err != nil {
		t.Errorf("manageMuPlugins() should not return error for non-existent parent, got %v", err)
	}

	// Verify the directory was not created
	if _, err := os.Stat(nonExistentPath); !os.IsNotExist(err) {
		t.Error("Directory should not have been created when parent doesn't exist")
	}
}
