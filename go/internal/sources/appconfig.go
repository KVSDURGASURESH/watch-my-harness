package sources

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// AppConfig is ~/.claude/token-dashboard-config.json. Licence is a placeholder:
// it is stored and shown, never enforced.
type AppConfig struct {
	Licence string `json:"licence"`
}

func AppConfigPath() string {
	if p := os.Getenv("TOKEN_DASHBOARD_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(home(), ".claude", "token-dashboard-config.json")
}

func LoadAppConfig() AppConfig {
	c := AppConfig{Licence: "free"}
	if b, err := os.ReadFile(AppConfigPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Licence == "" {
		c.Licence = "free"
	}
	return c
}
