package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func LoadFile(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	return Load(file)
}

func (c Config) LogPath(configPath string) string {
	if c.LogFile == "" || filepath.IsAbs(c.LogFile) {
		return c.LogFile
	}
	return filepath.Join(filepath.Dir(configPath), c.LogFile)
}

// Save replaces a validated configuration and retains the previous file as .bak.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing configuration: %w", err)
	}
	if err == nil {
		if err := writeAtomic(path+".bak", previous); err != nil {
			return fmt.Errorf("back up configuration: %w", err)
		}
	}
	if err := writeAtomic(path, data); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".local-ai-relay-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
