
package selective

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

type Rule struct {
	Pattern  string
	Exclude  bool
	Absolute bool
}

type List struct {
	Rules           []Rule
	IncludeAbs      []string
	IncludeAnywhere []string
	ExcludeAbs      []string
	ExcludeAnywhere []string
	HasRules        bool
}

func Load(path string) (*List, error) {
	l := &List{}
	if path == "" { return l, nil }
	f, err := os.Open(path)
	if err != nil { return l, nil }
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" { continue }
		if strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") { continue }
		ex := false
		if strings.HasPrefix(line, "!") || strings.HasPrefix(line, "-") {
			ex = true
			line = strings.TrimSpace(line[1:])
		}
		line = strings.ReplaceAll(line, "\\", "/")
		line = strings.TrimSpace(line)
		if line == "" || line == "/*" || line == "/" { continue }
		norm := line
		if strings.HasSuffix(norm, "/*") { norm = strings.TrimSuffix(norm, "/*") }
		r := Rule{ Pattern: norm, Exclude: ex, Absolute: strings.HasPrefix(norm, "/") }
		l.Rules = append(l.Rules, r)
		if r.Exclude {
			if r.Absolute { l.ExcludeAbs = append(l.ExcludeAbs, norm) } else { l.ExcludeAnywhere = append(l.ExcludeAnywhere, norm) }
		} else {
			if r.Absolute { l.IncludeAbs = append(l.IncludeAbs, norm) } else { l.IncludeAnywhere = append(l.IncludeAnywhere, norm) }
		}
	}
	l.HasRules = len(l.Rules) > 0
	return l, sc.Err()
}

func (l *List) ShouldSync(pathRel string, isDir bool) bool {
	if !l.HasRules { return true }
	p := "/" + strings.ReplaceAll(pathRel, "\\", "/")
	if l.matchExclude(p) { return false }
	if len(l.IncludeAbs)+len(l.IncludeAnywhere) > 0 {
		return l.matchInclude(p)
	}
	return true
}

func (l *List) matchExclude(p string) bool {
	for _, rule := range l.ExcludeAbs {
		if prefixMatch(p, rule) { return true }
	}
	base := filepath.Base(p)
	for _, rule := range l.ExcludeAnywhere {
		if matchAnywhere(p, base, rule) { return true }
	}
	return false
}

func (l *List) matchInclude(p string) bool {
	for _, rule := range l.IncludeAbs {
		if prefixMatch(p, rule) { return true }
	}
	base := filepath.Base(p)
	for _, rule := range l.IncludeAnywhere {
		if matchAnywhere(p, base, rule) { return true }
	}
	return false
}

func prefixMatch(p, rule string) bool {
	if !strings.HasSuffix(p, "/") { p = p + "/" }
	r := rule
	if !strings.HasSuffix(r, "/") { r = r + "/" }
	return strings.HasPrefix(p, r) || strings.HasPrefix(r, p)
}

func matchAnywhere(full, base, rule string) bool {
	if strings.HasSuffix(rule, "/*") {
		dir := strings.TrimSuffix(rule, "/*")
		parts := strings.Split(strings.Trim(full, "/"), "/")
		for _, part := range parts {
			if part == dir { return true }
		}
	}
	if strings.Contains(rule, "*") || strings.Contains(rule, "?") {
		if ok, _ := filepath.Match(rule, base); ok { return true }
		rel := strings.TrimPrefix(full, "/")
		if ok, _ := filepath.Match(rule, rel); ok { return true }
		return false
	}
	for _, seg := range strings.Split(strings.Trim(full, "/"), "/") {
		if seg == rule { return true }
	}
	return base == rule
}
