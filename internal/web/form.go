package web

import (
	"net/http"
	"strconv"
	"strings"
)

// 表单取值的几个小助手。所有后台表单都走这里，省得每处都判空。

func formValue(r *http.Request, key string) string {
	return strings.TrimSpace(r.PostFormValue(key))
}

func formInt(r *http.Request, key string, def int) int {
	v := formValue(r, key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func formBool(r *http.Request, key string) bool {
	v := r.PostFormValue(key)
	return v == "1" || v == "on" || v == "true"
}

// formIntList 解析多选出来的 id 列表。
func formIntList(r *http.Request, key string) ([]int64, error) {
	var out []int64
	for _, raw := range r.PostForm[key] {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
