// Package engine 是转发引擎：把入站事件按规则分发到目标源，并负责重试与循环检测。
package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/loarland/Forward2Any/internal/store"
)

// Match 判断载荷是否满足规则的全部过滤条件（多条之间 AND）。
// filters 为空视为放行。
func Match(filters []store.Filter, data any) bool {
	for _, f := range filters {
		if !matchOne(f, data) {
			return false
		}
	}
	return true
}

func matchOne(f store.Filter, data any) bool {
	got, found := lookup(data, f.Path)

	// 这两个操作符本来就用来判断「有没有」，缺字段是有意义的输入。
	switch f.Op {
	case "exists":
		return found
	case "not_exists":
		return !found
	}
	if !found {
		return false
	}

	switch f.Op {
	case "", "eq":
		return equal(got, f.Value)
	case "ne":
		return !equal(got, f.Value)
	case "gt":
		a, aok := toFloat(got)
		b, bok := toFloat(f.Value)
		return aok && bok && a > b
	case "lt":
		a, aok := toFloat(got)
		b, bok := toFloat(f.Value)
		return aok && bok && a < b
	case "contains":
		return contains(got, f.Value)
	case "not_contains":
		return !contains(got, f.Value)
	case "regex":
		re, err := regexp.Compile(fmt.Sprint(f.Value))
		if err != nil {
			return false
		}
		return re.MatchString(fmt.Sprint(got))
	case "in":
		list, ok := f.Value.([]any)
		if !ok {
			return false
		}
		for _, item := range list {
			if equal(got, item) {
				return true
			}
		}
		return false
	}
	return false
}

// lookup 按点路径取值，纯数字段当作数组下标：commits.0.message
func lookup(data any, path string) (any, bool) {
	if strings.TrimSpace(path) == "" {
		return nil, false
	}
	cur := data
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func equal(a, b any) bool {
	// JSON 里的数字都是 float64，而表单里填的可能是字符串，先按数值比一次。
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func contains(haystack, needle any) bool {
	switch h := haystack.(type) {
	case string:
		return strings.Contains(h, fmt.Sprint(needle))
	case []any:
		for _, item := range h {
			if equal(item, needle) {
				return true
			}
		}
		return false
	}
	return strings.Contains(fmt.Sprint(haystack), fmt.Sprint(needle))
}

// ParseFilterValue 把表单里的一行文本转成合适的类型。
//
// 先按 JSON 解析，于是 true / 123 / "abc" / [1,2] 都写得出来；
// 解析失败（比如裸词 push）就当普通字符串。
func ParseFilterValue(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}
