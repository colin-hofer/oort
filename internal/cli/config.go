package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type invocationOptions struct {
	Server  string
	Profile string
	Tenant  string
	JSON    bool
}

type profile struct {
	Server        string `json:"server"`
	DefaultTenant string `json:"default_tenant,omitempty"`
}

type userConfig struct {
	ActiveProfile string             `json:"active_profile"`
	Profiles      map[string]profile `json:"profiles"`
}

type credentials struct {
	Tokens map[string]string `json:"tokens"`
}

type projectContext struct {
	Profile string `json:"profile,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
}

var currentOptions invocationOptions

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "oort")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "oort-config")
	}
	return filepath.Join(home, ".config", "oort")
}

func loadUserConfig() (userConfig, credentials, error) {
	config := userConfig{ActiveProfile: "default", Profiles: map[string]profile{}}
	secrets := credentials{Tokens: map[string]string{}}
	if err := readOptionalJSON(filepath.Join(configDir(), "config.json"), &config); err != nil {
		return config, secrets, fmt.Errorf("read CLI config: %w", err)
	}
	if err := readOptionalJSON(filepath.Join(configDir(), "credentials.json"), &secrets); err != nil {
		return config, secrets, fmt.Errorf("read CLI credentials: %w", err)
	}
	if config.ActiveProfile == "" {
		config.ActiveProfile = "default"
	}
	if config.Profiles == nil {
		config.Profiles = map[string]profile{}
	}
	if secrets.Tokens == nil {
		secrets.Tokens = map[string]string{}
	}
	return config, secrets, nil
}

func saveUserConfig(config userConfig, secrets credentials) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "config.json"), config, 0o600); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "credentials.json"), secrets, 0o600)
}

func readProjectContext() (projectContext, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return projectContext{}, "", err
	}
	for {
		path := filepath.Join(dir, ".oort", "context.json")
		var context projectContext
		if err := readOptionalJSON(path, &context); err != nil {
			return projectContext{}, "", err
		} else if context.Profile != "" || context.Tenant != "" {
			return context, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return projectContext{}, "", nil
		}
		dir = parent
	}
}

func writeProjectContext(context projectContext) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for current := dir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "oort.json")); err == nil {
			dir = current
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	path := filepath.Join(dir, ".oort", "context.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, writeJSONFile(path, context, 0o600)
}

func activeProfile() (string, profile, userConfig, credentials, projectContext, error) {
	config, secrets, err := loadUserConfig()
	if err != nil {
		return "", profile{}, config, secrets, projectContext{}, err
	}
	project, _, err := readProjectContext()
	if err != nil {
		return "", profile{}, config, secrets, project, err
	}
	name := first(currentOptions.Profile, os.Getenv("OORT_PROFILE"), project.Profile, config.ActiveProfile, "default")
	return name, config.Profiles[name], config, secrets, project, nil
}

func readOptionalJSON(path string, value any) error {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func writeJSONFile(path string, value any, mode os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".oort-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
