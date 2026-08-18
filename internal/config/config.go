package config

import (
	"encoding/json"
	"os"
	"strconv"
)

type Config struct {
	Listen           string                 `json:"listen"`
	APIKey           string                 `json:"api_key"`
	Models           []string               `json:"models"`
	PoolMin          int                    `json:"pool_min"`
	PoolMax          int                    `json:"pool_max"`
	TTLMin           int                    `json:"ttl_minutes"`
	BindTTLMin       int                    `json:"bind_ttl_minutes"`
	MaxReqPerSession int                    `json:"max_req_per_session"`
	UpstreamURL      string                 `json:"upstream_url"`
	AgentPreset      string                 `json:"agent_preset"`
	ModelMap         map[string]ModelMapping `json:"model_map"`
}

// ModelMapping maps a client-facing model name to the upstream provider/model
// and optional reasoning effort ("off", "high", "max").
type ModelMapping struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// defaultModelMap covers models discovered via session.models on the minimal
// preset.  Only the edgeone-makers provider is usable without a EDGEONE_API_KEY;
// deepseek-official models require the credentials service and are excluded.
func defaultModelMap() map[string]ModelMapping {
	return map[string]ModelMapping{
		"@makers/hy3":                {Provider: "edgeone-makers", Model: "@makers/hy3"},
		"@makers/hy3-preview":        {Provider: "edgeone-makers", Model: "@makers/hy3-preview"},
		"@makers/deepseek-v4-pro":    {Provider: "edgeone-makers", Model: "@makers/deepseek-v4-pro"},
		"@makers/deepseek-v4-flash":  {Provider: "edgeone-makers", Model: "@makers/deepseek-v4-flash"},
		"@makers/minimax-m3":         {Provider: "edgeone-makers", Model: "@makers/minimax-m3"},
		"@makers/minimax-m2.7":       {Provider: "edgeone-makers", Model: "@makers/minimax-m2.7"},
		"@makers/kimi-k2.6":          {Provider: "edgeone-makers", Model: "@makers/kimi-k2.6"},
	}
}

func defaultConfig() Config {
	return Config{
		Listen:           ":7863",
		APIKey:           "",
		Models:           []string{"@makers/deepseek-v4-flash", "@makers/deepseek-v4-pro"},
		PoolMin:          2,
		PoolMax:          8,
		TTLMin:           60,
		BindTTLMin:       30,
		MaxReqPerSession: 200,
		UpstreamURL:      "https://deepseek-harness.edgeone.cool",
		AgentPreset:      "minimal",
		ModelMap:         defaultModelMap(),
	}
}

func Load(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
	}
	// Environment variable overrides
	if v := os.Getenv("EDGEONE_API_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("EDGEONE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("EDGEONE_API_MODELS"); v != "" {
		cfg.Models = splitAndTrim(v, ",")
	}
	if v := os.Getenv("EDGEONE_API_POOL_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMin = n
		}
	}
	if v := os.Getenv("EDGEONE_API_POOL_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMax = n
		}
	}
	if v := os.Getenv("EDGEONE_API_TTL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TTLMin = n
		}
	}
	if v := os.Getenv("EDGEONE_API_BIND_TTL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BindTTLMin = n
		}
	}
	if v := os.Getenv("EDGEONE_API_MAX_REQ_PER_SESSION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxReqPerSession = n
		}
	}
	if v := os.Getenv("EDGEONE_API_UPSTREAM"); v != "" {
		cfg.UpstreamURL = v
	}
	return cfg, nil
}

func splitAndTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	result := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if string(s[i]) == sep {
			if start < i {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	for i := range result {
		result[i] = trim(result[i])
	}
	return result
}

func trim(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}