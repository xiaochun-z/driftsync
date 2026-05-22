// Package selective implements include/exclude filtering rules for sync paths.
// Rules can be loaded from a text file (Load) or constructed from YAML config (FromYAML).
// Both sources share identical matching semantics.
package selective

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Rule represents a single include or exclude pattern.
type Rule struct {
	Pattern  string
	Exclude  bool
	Absolute bool // true when the pattern starts with "/" (anchored to sync root)
}

// List is the compiled set of include/exclude rules.
type List struct {
	Rules           []Rule
	IncludeAbs      []string // patterns anchored to root (start with "/")
	IncludeAnywhere []string // patterns matched at any depth
	ExcludeAbs      []string
	ExcludeAnywhere []string
	HasRules        bool
}

// normalizePath converts a raw rule string into canonical form:
// backslashes → forward slashes, trim whitespace, strip trailing "/*".
func normalizePath(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	s = strings.TrimSpace(s)
	return strings.TrimSuffix(s, "/*")
}

// add inserts a single already-normalized pattern into the list indexes.
func (l *List) add(pattern string, exclude bool) {
	if pattern == "" || pattern == "/*" || pattern == "/" {
		return
	}
	abs := strings.HasPrefix(pattern, "/")
	l.Rules = append(l.Rules, Rule{Pattern: pattern, Exclude: exclude, Absolute: abs})
	if exclude {
		if abs {
			l.ExcludeAbs = append(l.ExcludeAbs, pattern)
		} else {
			l.ExcludeAnywhere = append(l.ExcludeAnywhere, pattern)
		}
	} else {
		if abs {
			l.IncludeAbs = append(l.IncludeAbs, pattern)
		} else {
			l.IncludeAnywhere = append(l.IncludeAnywhere, pattern)
		}
	}
	l.HasRules = true
}

// Load reads a text sync-list file. An empty or missing path returns an empty list
// (no rules → allow everything). File format:
//   - Lines starting with "#" or ";" are comments.
//   - Lines starting with "-" or "!" are exclude rules.
//   - All other non-empty lines are include rules.
//   - A leading "/" anchors the pattern to the sync root; otherwise it matches at any depth.
func Load(path string) (*List, error) {
	l := &List{}
	if path == "" {
		return l, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return l, nil // missing file → empty list (allow everything)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		exclude := false
		if strings.HasPrefix(line, "!") || strings.HasPrefix(line, "-") {
			exclude = true
			line = strings.TrimSpace(line[1:])
		}
		l.add(normalizePath(line), exclude)
	}
	return l, sc.Err()
}

// FromYAML constructs a List from the include/exclude slices in a YAML config block.
// Matching semantics are identical to Load.
func FromYAML(include, exclude []string) *List {
	l := &List{}
	for _, raw := range exclude {
		l.add(normalizePath(raw), true)
	}
	for _, raw := range include {
		l.add(normalizePath(raw), false)
	}
	return l
}

// ShouldSync reports whether pathRel should participate in sync.
// Safe to call on a nil receiver — returns true (allow everything).
// Exclude rules take priority over include rules.
func (l *List) ShouldSync(pathRel string, isDir bool) bool {
	if l == nil || !l.HasRules {
		return true
	}
	rel := strings.ReplaceAll(pathRel, "\\", "/")
	rel = strings.TrimLeft(rel, "/")
	if isDir {
		rel = strings.TrimRight(rel, "/") + "/"
	}
	p := "/" + rel

	if l.matchExclude(p) {
		return false
	}
	if len(l.IncludeAbs)+len(l.IncludeAnywhere) > 0 {
		return l.matchInclude(p)
	}
	return true
}

func (l *List) matchExclude(p string) bool {
	for _, rule := range l.ExcludeAbs {
		if prefixMatch(p, rule) {
			return true
		}
	}
	base := filepath.Base(p)
	for _, rule := range l.ExcludeAnywhere {
		if matchAnywhere(p, base, rule) {
			return true
		}
	}
	return false
}

func (l *List) matchInclude(p string) bool {
	for _, rule := range l.IncludeAbs {
		if prefixMatch(p, rule) {
			return true
		}
	}
	base := filepath.Base(p)
	for _, rule := range l.IncludeAnywhere {
		if matchAnywhere(p, base, rule) {
			return true
		}
	}
	return false
}

func prefixMatch(p, rule string) bool {
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	if !strings.HasSuffix(rule, "/") {
		rule += "/"
	}
	return strings.HasPrefix(p, rule) || strings.HasPrefix(rule, p)
}

func matchAnywhere(full, base, rule string) bool {
	// Multi-segment directory rule ending with "/" (e.g. "obsidian/.obsidian/")
	if strings.HasSuffix(rule, "/") && !strings.HasSuffix(rule, "/*") {
		r := rule
		if !strings.HasPrefix(r, "/") {
			r = "/" + r
		}
		if strings.Contains(full, r) {
			return true
		}
	}
	if strings.HasSuffix(rule, "/*") {
		dir := strings.TrimSuffix(rule, "/*")
		for _, seg := range strings.Split(strings.Trim(full, "/"), "/") {
			if seg == dir {
				return true
			}
		}
	}
	if strings.Contains(rule, "*") || strings.Contains(rule, "?") {
		if ok, _ := filepath.Match(rule, base); ok {
			return true
		}
		if ok, _ := filepath.Match(rule, strings.TrimPrefix(full, "/")); ok {
			return true
		}
		return false
	}
	for _, seg := range strings.Split(strings.Trim(full, "/"), "/") {
		if seg == rule {
			return true
		}
	}
	return base == rule
}
