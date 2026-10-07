package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "/home/container/config.yml"
const DefaultStatePath = "/home/container/.inf-backup-protocol.json"

type Config struct {
	Repository      string
	TargetPath      string
	PasswordFile    string
	ResticPath      string
	StatePath       string
	TmpDir          string
	ExcludePatterns []string
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("无法读取配置文件: %w", err)
	}
	defer f.Close()
	decoder := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	var values map[string]any
	if err = decoder.Decode(&values); err != nil || values == nil {
		// YAML diagnostics can contain inline passwords or repository credentials.
		return Config{}, errors.New("配置文件必须是有效的 YAML 映射")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Config{}, errors.New("配置文件必须只包含一个 YAML 文档")
	}
	if _, exists := values["password"]; exists {
		return Config{}, errors.New("不支持内联 password；请配置 password_file")
	}
	get := func(key, fallback string) (string, error) {
		value, exists := values[key]
		if !exists && fallback != "" {
			return fallback, nil
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" || strings.ContainsRune(text, '\x00') {
			return "", fmt.Errorf("配置项 %s 必须是非空字符串", key)
		}
		return strings.TrimSpace(text), nil
	}
	cfg := Config{}
	for _, item := range []struct {
		key, fallback string
		dest          *string
	}{
		{"repository", "", &cfg.Repository}, {"target_path", "", &cfg.TargetPath},
		{"password_file", "", &cfg.PasswordFile}, {"restic_path", "restic", &cfg.ResticPath},
		{"state_path", DefaultStatePath, &cfg.StatePath},
	} {
		*item.dest, err = get(item.key, item.fallback)
		if err != nil {
			return Config{}, err
		}
	}
	if value, exists := values["tmp_dir"]; exists && value != nil {
		cfg.TmpDir, err = get("tmp_dir", "")
		if err != nil {
			return Config{}, err
		}
	}
	if value, exists := values["exclude_patterns"]; exists {
		patterns, ok := value.([]any)
		if !ok {
			return Config{}, errors.New("配置项 exclude_patterns 必须是非空字符串列表")
		}
		for _, pattern := range patterns {
			text, ok := pattern.(string)
			if !ok || text == "" || strings.ContainsRune(text, '\x00') {
				return Config{}, errors.New("配置项 exclude_patterns 必须是非空字符串列表")
			}
			cfg.ExcludePatterns = append(cfg.ExcludePatterns, text)
		}
	}
	return cfg, nil
}

// Environment supplies credentials through files, never command-line arguments.
func (c Config) Environment() ([]string, error) {
	info, err := os.Stat(c.PasswordFile)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("password_file 不存在或不是普通文件")
	}
	if c.TmpDir != "" {
		if err = os.MkdirAll(c.TmpDir, 0700); err != nil {
			return nil, errors.New("无法创建 tmp_dir；请检查目录权限")
		}
	}
	env := []string{}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND":
			continue
		case "TMPDIR":
			if c.TmpDir != "" {
				continue
			}
		}
		env = append(env, value)
	}
	env = append(env, "RESTIC_REPOSITORY="+c.Repository, "RESTIC_PASSWORD_FILE="+c.PasswordFile)
	if c.TmpDir != "" {
		env = append(env, "TMPDIR="+c.TmpDir)
	}
	return env, nil
}

func Tags(tags []string) ([]string, error) {
	result := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		if tag == "" || strings.ContainsFunc(tag, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return nil, errors.New("备份标签不能为空或包含空白、控制字符")
		}
		result = append(result, tag)
	}
	return result, nil
}

var urlCredentials = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`)

func (c Config) Redact(text string) string {
	if c.Repository != "" {
		text = strings.ReplaceAll(text, c.Repository, "[repository]")
	}
	return urlCredentials.ReplaceAllString(text, "$1[redacted]@")
}
