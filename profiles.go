package main

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed packaging/profiles.yaml
var defaultProfiles embed.FS

type Profile struct {
	Name      string
	Agent     string
	Model     string
	Effort    string
	TimeLimit time.Duration
}

var profileOrder = []string{"codex-quick", "claude-quick", "codex-std", "claude-std", "codex-deep", "claude-epm", "claude-deep"}

func parseProfiles(data []byte) (map[string]Profile, error) {
	profiles := map[string]Profile{}
	name := ""
	seen := map[string]bool{}
	header := false
	for n, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if raw == "profiles:" {
			if header || len(profiles) != 0 {
				return nil, errors.New("duplicate profiles header")
			}
			header = true
			continue
		}
		if !header {
			return nil, errors.New("missing profiles header")
		}
		if strings.HasPrefix(raw, "  ") && !strings.HasPrefix(raw, "    ") && strings.HasSuffix(raw, ":") {
			name = strings.TrimSuffix(strings.TrimSpace(raw), ":")
			if !taskName.MatchString(name) || seen[name] {
				return nil, fmt.Errorf("invalid or duplicate profile on line %d", n+1)
			}
			seen[name] = true
			profiles[name] = Profile{Name: name}
			continue
		}
		if !strings.HasPrefix(raw, "    ") || strings.HasPrefix(raw, "     ") || name == "" {
			return nil, fmt.Errorf("invalid profile indentation on line %d", n+1)
		}
		key, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid profile field on line %d", n+1)
		}
		value = strings.TrimSpace(value)
		p := profiles[name]
		switch key {
		case "agent":
			if p.Agent != "" {
				return nil, errors.New("duplicate agent")
			}
			p.Agent = value
		case "model":
			if p.Model != "" {
				return nil, errors.New("duplicate model")
			}
			p.Model = value
		case "effort":
			if p.Effort != "" {
				return nil, errors.New("duplicate effort")
			}
			p.Effort = value
		case "time_limit":
			if p.TimeLimit != 0 {
				return nil, errors.New("duplicate time_limit")
			}
			var err error
			p.TimeLimit, err = time.ParseDuration(value)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown profile field %q", key)
		}
		profiles[name] = p
	}
	if len(profiles) == 0 {
		return nil, errors.New("no profiles")
	}
	for name, p := range profiles {
		if p.Agent != "claude" && p.Agent != "codex" || p.Model == "" || strings.ContainsAny(p.Model, " \t\n") || p.TimeLimit <= 0 || p.Effort != "low" && p.Effort != "medium" && p.Effort != "high" {
			return nil, fmt.Errorf("invalid profile %q", name)
		}
	}
	return profiles, nil
}

func loadProfiles() (map[string]Profile, error) {
	if override := os.Getenv("AGENT_BRIDGE_PROFILES"); override != "" {
		b, err := os.ReadFile(override)
		if err != nil {
			return nil, err
		}
		return parseProfiles(b)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".config", "agent-bridge", "profiles.yaml")
	if b, err := os.ReadFile(path); err == nil {
		return parseProfiles(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	b, err := defaultProfiles.ReadFile("packaging/profiles.yaml")
	if err != nil {
		return nil, err
	}
	return parseProfiles(b)
}

func profileRank(name string) int {
	for i, candidate := range profileOrder {
		if name == candidate {
			return i
		}
	}
	return -1
}
func defaultProfile(agent, role string) string {
	if role == "epm" {
		return "claude-epm"
	}
	if agent == "claude" {
		return "claude-std"
	}
	return "codex-std"
}
func profileAllowed(name string, fromJob, userOverride bool) bool {
	if userOverride {
		return true
	}
	if name == "claude-deep" {
		return false
	}
	if fromJob && name == "claude-epm" {
		return true
	}
	return true
}
