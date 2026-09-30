package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen       string
	ComfyURL     string
	DataDir      string
	APIKey       string
	PollInterval time.Duration
	RunTimeout   time.Duration
}

// Load reads configuration from the environment, after merging a .env file
// from the working directory if there is one. Real environment variables win
// over the file.
func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	c := Config{
		Listen:   getenv("COMFYVAULT_LISTEN", "127.0.0.1:8080"),
		ComfyURL: strings.TrimRight(getenv("COMFY_URL", "http://127.0.0.1:8188"), "/"),
		DataDir:  getenv("COMFYVAULT_DATA", "./data"),
		APIKey:   os.Getenv("COMFYVAULT_API_KEY"),
	}
	var err error
	if c.PollInterval, err = duration("COMFYVAULT_POLL_INTERVAL", time.Second); err != nil {
		return c, err
	}
	if c.RunTimeout, err = duration("COMFYVAULT_RUN_TIMEOUT", 30*time.Minute); err != nil {
		return c, err
	}
	u, err := url.Parse(c.ComfyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("COMFY_URL must be an http(s) URL, got %q", c.ComfyURL)
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func duration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 500ms or 2m, got %q", key, raw)
	}
	return d, nil
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
