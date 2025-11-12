package selective

import (
	"strings"
)

// FromYAML 构造与文本 sync_list 同等语义的过滤器：
// - 以 "/" 开头 => 绝对前缀匹配（目录/文件，子树均生效；与 prefixMatch 一致）
// - 不以 "/" 开头 => 在任意层级匹配（支持 *.md、a/*、单段名等；与 matchAnywhere 一致）
func FromYAML(include, exclude []string) *List {
	l := &List{}

	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "\\", "/")
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "/*")
		return s
	}

	for _, raw := range exclude {
		p := normalize(raw)
		if p == "" || p == "/*" || p == "/" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			l.ExcludeAbs = append(l.ExcludeAbs, p)
		} else {
			l.ExcludeAnywhere = append(l.ExcludeAnywhere, p)
		}
		l.Rules = append(l.Rules, Rule{Pattern: p, Exclude: true, Absolute: strings.HasPrefix(p, "/")})
	}

	for _, raw := range include {
		p := normalize(raw)
		if p == "" || p == "/*" || p == "/" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			l.IncludeAbs = append(l.IncludeAbs, p)
		} else {
			l.IncludeAnywhere = append(l.IncludeAnywhere, p)
		}
		l.Rules = append(l.Rules, Rule{Pattern: p, Exclude: false, Absolute: strings.HasPrefix(p, "/")})
	}

	l.HasRules = len(l.Rules) > 0
	return l
}
