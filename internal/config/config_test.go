package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndRejectUnsafeConfig(t *testing.T) {
	base := "repository: /offline/repository\ntarget_path: /offline/target\npassword_file: /offline/password\n"
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"defaults", base, true},
		{"optional", base + "tmp_dir: /offline/tmp\nexclude_patterns: [cache/**]\n", true},
		{"inline", base + "password: do-not-echo\n", false},
		{"missing", "repository: /offline/repository\n", false},
		{"number", strings.Replace(base, "/offline/target", "123", 1), false},
		{"list", "[do-not-echo]", false},
		{"yaml", "repository: [do-not-echo", false},
		{"excludes", base + "exclude_patterns: [123]\n", false},
		{"null-excludes", base + "exclude_patterns: null\n", false},
		{"two-documents", base + "---\npassword: do-not-echo\n", false},
		{"duplicate", base + "repository: do-not-echo\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("configuration diagnostic leaked input")
			}
			if tc.valid && (cfg.ResticPath != "restic" || cfg.StatePath != DefaultStatePath) {
				t.Fatal("defaults changed")
			}
		})
	}
}

func TestEnvironmentUsesFilesAndRemovesConflictingCredentials(t *testing.T) {
	root := t.TempDir()
	password := filepath.Join(root, "password")
	if err := os.WriteFile(password, []byte("offline-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTIC_PASSWORD", "unused-placeholder")
	t.Setenv("RESTIC_PASSWORD_COMMAND", "unused-command")
	t.Setenv("RESTIC_REPOSITORY_FILE", "/unused")
	t.Setenv("RESTIC_REPOSITORY", "/unused")
	t.Setenv("TMPDIR", root)
	cfg := Config{Repository: "/offline/repository", PasswordFile: password, TmpDir: filepath.Join(root, "tmp")}
	env, err := cfg.Environment()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for _, key := range []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND", "RESTIC_REPOSITORY_FILE"} {
		if _, exists := values[key]; exists {
			t.Fatal("conflicting credential source retained:", key)
		}
	}
	if values["RESTIC_REPOSITORY"] != cfg.Repository || values["RESTIC_PASSWORD_FILE"] != password || values["TMPDIR"] != cfg.TmpDir {
		t.Fatal("Restic environment does not match configuration")
	}
	if info, err := os.Stat(cfg.TmpDir); err != nil || !info.IsDir() {
		t.Fatal("temporary directory not created")
	}
	cfg.PasswordFile = root
	if _, err = cfg.Environment(); err == nil {
		t.Fatal("directory accepted as password file")
	}
}

func TestTagsRemainValuesAndRedaction(t *testing.T) {
	for _, tag := range []string{"", "a b", "a\x00b", "a\u0085b", "a\x7fb"} {
		if _, err := Tags([]string{tag}); err == nil {
			t.Fatalf("invalid tag %q accepted", tag)
		}
	}
	if tags, err := Tags([]string{"--dry-run", "中文"}); err != nil || len(tags) != 2 {
		t.Fatal("literal tags rejected")
	}
	cfg := Config{Repository: "s3:https://example.invalid/private-bucket"}
	text := cfg.Redact("s3:https://example.invalid/private-bucket https://user:placeholder@example.invalid/file")
	if strings.Contains(text, "private-bucket") || strings.Contains(text, "placeholder") {
		t.Fatal("repository credentials leaked")
	}
}
