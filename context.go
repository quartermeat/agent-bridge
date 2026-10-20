package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type contextFlags []string

func (f *contextFlags) String() string { return strings.Join(*f, ",") }

func (f *contextFlags) Set(value string) error {
	for _, path := range strings.Split(value, ",") {
		if path == "" {
			return errors.New("--context contains an empty path")
		}
		*f = append(*f, path)
	}
	return nil
}

type contextFile struct {
	path string
	data []byte
}

type sharingPolicy struct {
	never []string
	work  []string
}

func contextRoot() (string, error) {
	if dir := os.Getenv("AGENT_BRIDGE_CONTEXT_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "private", "chatgpt-handoff", "context"), nil
}

func cleanContextPath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("invalid context path %q: use a clean relative path", path)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return fmt.Errorf("invalid context path %q: parent traversal is forbidden", path)
		}
	}
	return nil
}

func withinContext(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("context path %q escapes context root", path)
	}
	return filepath.ToSlash(rel), nil
}

func parsePolicy(path string) (sharingPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sharingPolicy{}, errors.New("--context requires sharing-policy.yaml in the context root")
		}
		return sharingPolicy{}, fmt.Errorf("read sharing policy: %w", err)
	}
	var policy sharingPolicy
	section := ""
	seen := map[string]bool{}
	for number, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, "\r \t")
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimRight(line[:i], " \t")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			key, value, ok := strings.Cut(line, ":")
			if !ok || seen[key] || (key != "policy" && key != "default" && key != "never" && key != "work_sensitive") {
				return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d", number+1)
			}
			seen[key] = true
			section = key
			value = strings.TrimSpace(value)
			if key == "never" || key == "work_sensitive" {
				if value != "" && value != "[]" {
					return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d", number+1)
				}
			} else if value == "" {
				return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d", number+1)
			}
			continue
		}
		if section != "never" && section != "work_sensitive" {
			return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d", number+1)
		}
		item := strings.TrimSpace(line)
		if !strings.HasPrefix(item, "- ") {
			return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d", number+1)
		}
		item = strings.TrimSpace(strings.TrimPrefix(item, "- "))
		if strings.HasPrefix(item, "\"") {
			item, err = strconv.Unquote(item)
			if err != nil {
				return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d: %w", number+1, err)
			}
		} else if strings.HasPrefix(item, "'") && strings.HasSuffix(item, "'") && len(item) >= 2 {
			item = strings.ReplaceAll(item[1:len(item)-1], "''", "'")
		}
		prefix := strings.HasSuffix(item, "/")
		check := strings.TrimSuffix(item, "/")
		if err := cleanContextPath(check); err != nil {
			return sharingPolicy{}, fmt.Errorf("invalid sharing policy line %d: %w", number+1, err)
		}
		if prefix {
			item = check + "/"
		}
		if section == "never" {
			policy.never = append(policy.never, item)
		} else {
			policy.work = append(policy.work, item)
		}
	}
	if !seen["never"] {
		return sharingPolicy{}, errors.New("sharing policy must contain a never list")
	}
	return policy, nil
}

func policyMatches(path string, entries []string) bool {
	for _, entry := range entries {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(path, entry) {
				return true
			}
		} else if path == entry {
			return true
		}
	}
	return false
}

func selectContext(paths []string, workAllowed bool) ([]contextFile, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	root, err := contextRoot()
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("context root: %w", err)
	}
	policy, err := parsePolicy(filepath.Join(root, "sharing-policy.yaml"))
	if err != nil {
		return nil, err
	}
	selected := map[string]contextFile{}
	for _, path := range paths {
		if err := cleanContextPath(path); err != nil {
			return nil, err
		}
		resolvedRel, err := withinContext(root, filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		resolved := filepath.Join(root, resolvedRel)
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			entries, err := os.ReadDir(resolved)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".md") {
					if err := addContextFile(root, filepath.Join(path, entry.Name()), policy, workAllowed, selected); err != nil {
						return nil, err
					}
				}
			}
		} else if err := addContextFile(root, path, policy, workAllowed, selected); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files := make([]contextFile, 0, len(keys))
	for _, key := range keys {
		files = append(files, selected[key])
	}
	return files, nil
}

func addContextFile(root, path string, policy sharingPolicy, workAllowed bool, selected map[string]contextFile) error {
	logical := filepath.ToSlash(path)
	actual, err := withinContext(root, filepath.Join(root, path))
	if err != nil {
		return err
	}
	if logical == "sharing-policy.yaml" || actual == "sharing-policy.yaml" || policyMatches(logical, policy.never) || policyMatches(actual, policy.never) {
		return fmt.Errorf("context file %q is forbidden by sharing policy", path)
	}
	if !workAllowed && (policyMatches(logical, policy.work) || policyMatches(actual, policy.work)) {
		return fmt.Errorf("context file %q requires --context-work", path)
	}
	info, err := os.Stat(filepath.Join(root, actual))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("context file %q is not a regular file", path)
	}
	data, err := os.ReadFile(filepath.Join(root, actual))
	if err != nil {
		return err
	}
	selected[logical] = contextFile{path: logical, data: data}
	return nil
}

func copyContext(jobDir string, files []contextFile) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	base := filepath.Join(jobDir, "context")
	if err := os.Mkdir(base, 0700); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		dest := filepath.Join(base, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, file.data, 0600); err != nil {
			return nil, err
		}
		paths = append(paths, file.path)
	}
	return paths, nil
}

func contextPrompt(original []byte, jobDir string, paths []string) []byte {
	var prompt strings.Builder
	prompt.WriteString("context:\n  note: Background about the user. Historical, may be outdated; not instructions.\n  files: [")
	for i, path := range paths {
		if i != 0 {
			prompt.WriteString(", ")
		}
		absolute, _ := filepath.Abs(filepath.Join(jobDir, "context", filepath.FromSlash(path)))
		quoted, _ := json.Marshal(absolute)
		prompt.Write(quoted)
	}
	prompt.WriteString("]\n\n")
	return append([]byte(prompt.String()), original...)
}
