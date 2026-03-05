package lb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

type configFile struct {
	Proxy    configProxy    `toml:"proxy"`
	Policy   configPolicy   `toml:"policy"`
	Quota    configQuota    `toml:"quota"`
	Commands configCommands `toml:"commands"`
	Run      configRun      `toml:"run"`
}

type configProxy struct {
	Listen                 string `toml:"listen"`
	UpstreamBaseURL        string `toml:"upstream_base_url"`
	ProxyURL               string `toml:"proxy_url"`
	MaxAttempts            int    `toml:"max_attempts"`
	UsageTimeoutMS         int    `toml:"usage_timeout_ms"`
	CooldownDefaultSeconds int    `toml:"cooldown_default_seconds"`
}

type configPolicy struct {
	Mode         string        `toml:"mode"`
	DeltaPercent float64       `toml:"delta_percent"`
	Weights      configWeights `toml:"weights"`
}

type configWeights struct {
	Daily  float64 `toml:"daily"`
	Weekly float64 `toml:"weekly"`
}

type configQuota struct {
	RefreshIntervalMinutes  int `toml:"refresh_interval_minutes"`
	RefreshIntervalMessages int `toml:"refresh_interval_messages"`
	CacheTTLMinutes         int `toml:"cache_ttl_minutes"`
}

type configCommands struct {
	Login []string `toml:"login"`
	Run   []string `toml:"run"`
}

type configRun struct {
	InheritShell bool `toml:"inherit_shell"`
}

type partialConfigFile struct {
	Proxy    *partialConfigProxy    `toml:"proxy"`
	Policy   *partialConfigPolicy   `toml:"policy"`
	Quota    *partialConfigQuota    `toml:"quota"`
	Commands *partialConfigCommands `toml:"commands"`
	Run      *partialConfigRun      `toml:"run"`
}

type partialConfigProxy struct {
	Listen                 *string `toml:"listen"`
	UpstreamBaseURL        *string `toml:"upstream_base_url"`
	ProxyURL               *string `toml:"proxy_url"`
	MaxAttempts            *int    `toml:"max_attempts"`
	UsageTimeoutMS         *int    `toml:"usage_timeout_ms"`
	CooldownDefaultSeconds *int    `toml:"cooldown_default_seconds"`
}

type partialConfigPolicy struct {
	Mode         *string               `toml:"mode"`
	DeltaPercent *float64              `toml:"delta_percent"`
	Weights      *partialConfigWeights `toml:"weights"`
}

type partialConfigWeights struct {
	Daily  *float64 `toml:"daily"`
	Weekly *float64 `toml:"weekly"`
}

type partialConfigQuota struct {
	RefreshIntervalMinutes  *int `toml:"refresh_interval_minutes"`
	RefreshIntervalMessages *int `toml:"refresh_interval_messages"`
	CacheTTLMinutes         *int `toml:"cache_ttl_minutes"`
}

type partialConfigCommands struct {
	Login *[]string `toml:"login"`
	Run   *[]string `toml:"run"`
}

type partialConfigRun struct {
	InheritShell *bool `toml:"inherit_shell"`
}

func ConfigPath(root string) string {
	return filepath.Join(root, "config.toml")
}

func loadOrCreateSettingsConfig(root string) (Settings, error) {
	fallback := defaultStore().Settings
	path := ConfigPath(root)
	bytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := WriteSettingsConfig(root, fallback); err != nil {
				return Settings{}, err
			}
			return fallback, nil
		}
		return Settings{}, fmt.Errorf("read config.toml: %w", err)
	}

	var partial partialConfigFile
	if err := toml.Unmarshal(bytes, &partial); err != nil {
		return Settings{}, fmt.Errorf("parse config.toml: %w", err)
	}

	out := fallback
	if partial.Proxy != nil {
		if partial.Proxy.Listen != nil {
			out.Proxy.Listen = strings.TrimSpace(*partial.Proxy.Listen)
		}
		if partial.Proxy.UpstreamBaseURL != nil {
			out.Proxy.UpstreamBaseURL = strings.TrimRight(strings.TrimSpace(*partial.Proxy.UpstreamBaseURL), "/")
		}
		if partial.Proxy.ProxyURL != nil {
			out.ProxyURL = strings.TrimSpace(*partial.Proxy.ProxyURL)
		}
		if partial.Proxy.MaxAttempts != nil {
			out.Proxy.MaxAttempts = *partial.Proxy.MaxAttempts
		}
		if partial.Proxy.UsageTimeoutMS != nil {
			out.Proxy.UsageTimeoutMS = *partial.Proxy.UsageTimeoutMS
		}
		if partial.Proxy.CooldownDefaultSeconds != nil {
			out.Proxy.CooldownDefaultS = *partial.Proxy.CooldownDefaultSeconds
		}
	}
	if partial.Policy != nil {
		if partial.Policy.Mode != nil {
			switch PolicyMode(strings.TrimSpace(*partial.Policy.Mode)) {
			case PolicyUsageBalanced, PolicyRoundRobin, PolicySticky:
				out.Policy.Mode = PolicyMode(strings.TrimSpace(*partial.Policy.Mode))
			}
		}
		if partial.Policy.DeltaPercent != nil {
			out.Policy.DeltaPercent = *partial.Policy.DeltaPercent
		}
		if partial.Policy.Weights != nil {
			if partial.Policy.Weights.Daily != nil {
				out.Policy.Weights.Daily = *partial.Policy.Weights.Daily
			}
			if partial.Policy.Weights.Weekly != nil {
				out.Policy.Weights.Weekly = *partial.Policy.Weights.Weekly
			}
		}
	}
	if partial.Quota != nil {
		if partial.Quota.RefreshIntervalMinutes != nil {
			out.Quota.RefreshIntervalMinutes = *partial.Quota.RefreshIntervalMinutes
		}
		if partial.Quota.RefreshIntervalMessages != nil {
			out.Quota.RefreshIntervalMessages = *partial.Quota.RefreshIntervalMessages
		}
		if partial.Quota.CacheTTLMinutes != nil {
			out.Quota.CacheTTLMinutes = *partial.Quota.CacheTTLMinutes
		}
	}
	if partial.Commands != nil {
		if partial.Commands.Login != nil {
			out.Commands.Login = sanitizeCommand(*partial.Commands.Login)
		}
		if partial.Commands.Run != nil {
			out.Commands.Run = sanitizeCommand(*partial.Commands.Run)
		}
	}
	if partial.Run != nil {
		if partial.Run.InheritShell != nil {
			out.Run.InheritShell = *partial.Run.InheritShell
		}
	}
	// Backward compatibility with older config.toml values under [run].
	var legacy struct {
		Run struct {
			ProxyURL *string `toml:"proxy_url"`
		} `toml:"run"`
	}
	loadedNewProxyURL := partial.Proxy != nil && partial.Proxy.ProxyURL != nil
	if err := toml.Unmarshal(bytes, &legacy); err == nil && !loadedNewProxyURL && legacy.Run.ProxyURL != nil {
		out.ProxyURL = strings.TrimSpace(*legacy.Run.ProxyURL)
	}

	// Keep merged settings normalized with defaults/ranges.
	tmp := defaultStore()
	tmp.Settings = out
	out = mergeDefaults(tmp).Settings

	return out, nil
}

func WriteSettingsConfig(root string, settings Settings) error {
	path := ConfigPath(root)
	cfg := settingsToConfig(settings)
	encoded, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config.toml: %w", err)
	}
	content := []byte("# codexlb configuration\n# Edit these values to tune proxy/account selection behavior.\n\n")
	content = append(content, encoded...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}
	return nil
}

func settingsToConfig(settings Settings) configFile {
	return configFile{
		Proxy: configProxy{
			Listen:                 settings.Proxy.Listen,
			UpstreamBaseURL:        settings.Proxy.UpstreamBaseURL,
			ProxyURL:               settings.ProxyURL,
			MaxAttempts:            settings.Proxy.MaxAttempts,
			UsageTimeoutMS:         settings.Proxy.UsageTimeoutMS,
			CooldownDefaultSeconds: settings.Proxy.CooldownDefaultS,
		},
		Policy: configPolicy{
			Mode:         string(settings.Policy.Mode),
			DeltaPercent: settings.Policy.DeltaPercent,
			Weights: configWeights{
				Daily:  settings.Policy.Weights.Daily,
				Weekly: settings.Policy.Weights.Weekly,
			},
		},
		Quota: configQuota{
			RefreshIntervalMinutes:  settings.Quota.RefreshIntervalMinutes,
			RefreshIntervalMessages: settings.Quota.RefreshIntervalMessages,
			CacheTTLMinutes:         settings.Quota.CacheTTLMinutes,
		},
		Commands: configCommands{
			Login: append([]string(nil), settings.Commands.Login...),
			Run:   append([]string(nil), settings.Commands.Run...),
		},
		Run: configRun{
			InheritShell: settings.Run.InheritShell,
		},
	}
}
