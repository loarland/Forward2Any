package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinBytes 以下不压：小响应压完可能更大，还白搭一次 CPU。
// 静态资源都远大于这个值，动态 HTML 一般没有 Content-Length，会照压。
const gzipMinBytes = 1024

// gzipPool 复用压缩器，免得每个响应都新分配一个。
var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// gzipIfAccepted 给整个 handler 树套一层 gzip。
//
// 之所以要拦 WriteHeader 而不是简单地「包一下 Write」，是因为 http.ServeContent
// （静态文件走的就是它）会自己设 Content-Length。如果不等它设完就把
// Content-Encoding 挂上去，浏览器会按未压缩的长度去读 gzip 流 —— 响应直接坏掉。
// 所以决定必须发生在响应头定稿的那一刻，也就是 WriteHeader。
//
// 另外三种情况不能压：
//   - 已经有 Content-Encoding 的（上游压过了，别压第二层）；
//   - 1xx / 204 / 304（按规范没有响应体，也就不该有 Content-Encoding）；
//   - Range 请求（ServeContent 要自己处理 206 和 Content-Range，改写会破坏语义）。
type gzipResponse struct {
	http.ResponseWriter

	req         *http.Request
	gz          *gzip.Writer
	compress    bool
	wroteHeader bool
}

func (w *gzipResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.decide(status)
	w.ResponseWriter.WriteHeader(status)
}

// decide 在响应头定稿前判断要不要压，并在要压时把 Content-Length 摘掉
// —— 压缩后长度变了，留着会让浏览器截断或一直等。
func (w *gzipResponse) decide(status int) {
	h := w.Header()
	if h.Get("Content-Encoding") != "" {
		return
	}
	if (status >= 100 && status < 200) || status == http.StatusNoContent || status == http.StatusNotModified {
		return
	}
	if w.req.Header.Get("Range") != "" {
		return
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < gzipMinBytes {
		return
	}
	w.compress = true
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
}

func (w *gzipResponse) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.compress {
		return w.ResponseWriter.Write(p)
	}
	if w.gz == nil {
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	return w.gz.Write(p)
}

// close 收尾。必须由中间件在处理器返回后调用：gzip 流的尾巴（以及校验和）
// 是在 Close 里写出去的，漏了的话浏览器拿到的是截断的响应。
// 显式声明 Flush 会绕过压缩器，这里不实现 Flush，让 net/http 走默认路径。
func (w *gzipResponse) close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipPool.Put(w.gz)
	w.gz = nil
}

func gzipIfAccepted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 同一个 URL 压不压取决于请求头，缓存必须按这个区分，所以无论压不压都写上。
		w.Header().Add("Vary", "Accept-Encoding")

		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gr := &gzipResponse{ResponseWriter: w, req: r}
		defer gr.close()
		next.ServeHTTP(gr, r)
	})
}

// acceptsGzip 只看有没有 gzip。
//
// 没有按规范解析 q 值：本站的客户端是浏览器和 curl，不会写「q=0 明确拒绝 gzip」
// 这种形式，为它写一个完整的 Accept-Encoding 解析器不划算。
func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}
