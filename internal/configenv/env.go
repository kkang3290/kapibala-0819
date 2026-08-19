package configenv

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// String reads KEY_FILE when configured, otherwise KEY, then fallback. File
// values are trimmed so Docker secret files can end with a newline.
func String(key, fallback string) (string, error) {
	if path := os.Getenv(key + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", key, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("%s_FILE is empty", key)
		}
		return value, nil
	}
	if value := os.Getenv(key); value != "" {
		return value, nil
	}
	return fallback, nil
}

func Duration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}
