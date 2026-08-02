package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Bundle struct {
	mu           sync.RWMutex
	translations map[string]map[string]string
	defaultLang  string
}

var (
	globalBundle *Bundle
	once         sync.Once
)

// Load is an alias or wrapper for Init that returns a *Bundle, matching the expected signature.
func Load(localesDir string) (*Bundle, error) {
	if err := Init(localesDir); err != nil {
		return nil, err
	}
	return globalBundle, nil
}

// Init initializes the global i18n bundle by loading locale files from the given directory.
func Init(localesDir string) error {
	var err error
	once.Do(func() {
		b := &Bundle{
			translations: make(map[string]map[string]string),
			defaultLang:  "en",
		}

		// Read files in localesDir
		files, readErr := os.ReadDir(localesDir)
		if readErr != nil {
			err = fmt.Errorf("failed to read locales dir: %w", readErr)
			return
		}

		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}

			lang := strings.TrimSuffix(file.Name(), ".json")
			filePath := filepath.Join(localesDir, file.Name())

			data, readErr := os.ReadFile(filePath)
			if readErr != nil {
				err = fmt.Errorf("failed to read locale file %s: %w", filePath, readErr)
				return
			}

			var m map[string]string
			if unmarshalErr := json.Unmarshal(data, &m); unmarshalErr != nil {
				err = fmt.Errorf("failed to parse locale file %s: %w", filePath, unmarshalErr)
				return
			}

			b.translations[lang] = m
		}

		globalBundle = b
	})

	return err
}

// T translates a given key based on the language code. Falls back to defaultLang ("en") if missing.
func T(lang, key string) string {
	if globalBundle == nil {
		return key
	}

	globalBundle.mu.RLock()
	defer globalBundle.mu.RUnlock()

	// Normalize lang code (e.g. "en-US" -> "en")
	if idx := strings.Index(lang, "-"); idx != -1 {
		lang = lang[:idx]
	}
	lang = strings.ToLower(lang)

	if dict, ok := globalBundle.translations[lang]; ok {
		if val, ok := dict[key]; ok {
			return val
		}
	}

	// Fallback to default language
	if dict, ok := globalBundle.translations[globalBundle.defaultLang]; ok {
		if val, ok := dict[key]; ok {
			return val
		}
	}

	return key
}
