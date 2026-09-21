package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

const (
	drainBatch     = 20
	maxDrainRounds = 100
	pollInterval   = 2 * time.Second
	clientTimeout  = 30 * time.Second
	maxRespBytes   = 32 << 10
)

// Engine 负责把入站事件按规则分发出去，并异步投递、失败重试。
type Engine struct {
	store  *store.Store
	log    *slog.Logger
	client *http.Client

	notify chan struct{}
	stop   chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	running bool
	proxied map[string]*http.Client // 按代理地址缓存的客户端，受 mu 保护
}

func New(st *store.Store, log *slog.Logger) *Engine {
	return &Engine{
		store:  st,
		log:    log,
		client: &http.Client{Timeout: clientTimeout},
		notify: make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
}

// clientFor 返回这次投递要用的 HTTP 客户端：配了代理就走代理，没配就直连。
//
// ponytail: 按代理地址缓存客户端，不设上限 —— 地址只来自设置页那一个输入框，
// 条目数实际是常数；真要按地址无限增长，再加淘汰。
func (e *Engine) clientFor(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return e.client, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.proxied[proxyURL]; ok {
		return c, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("代理地址 %q 不合法: %w", proxyURL, err)
	}
	// 从默认传输克隆，免得只因为加了代理就丢掉超时、HTTP/2 这些默认值。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	c := &http.Client{Timeout: clientTimeout, Transport: tr}

	if e.proxied == nil {
		e.proxied = make(map[string]*http.Client)
	}
	e.proxied[proxyURL] = c
	return c, nil
}

// httpClientFor 返回这次投递要用的客户端。
// 只有「用途包含发送」的源才认代理勾选：用途改成纯接收之后，即使库里还留着这个标记，
// 也不该再走代理。邮件（SMTP）不走这里，所以代理配置只会影响 Webhook 和 Telegram。
func (e *Engine) httpClientFor(out *store.Source, settings *store.Settings) (*http.Client, error) {
	if !out.UseProxy || !out.CanSend() {
		return e.client, nil
	}
	return e.clientFor(settings.ProxyURL())
}

func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.mu.Unlock()

	e.wg.Add(1)
	go e.worker()
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	e.mu.Unlock()

	close(e.stop)
	e.wg.Wait()
}

// wake 立刻叫醒投递 worker，不等轮询间隔。
func (e *Engine) wake() {
	select {
	case e.notify <- struct{}{}:
	default:
	}
}

// Wake 供外部（比如后台的「重放」按钮）主动触发一次投递。
func (e *Engine) Wake() { e.wake() }

func (e *Engine) worker() {
	defer e.wg.Done()

	// 启动时先跑一轮：把上个进程遗留的 pending 记录接过来。
	e.drain()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-e.notify:
		case <-ticker.C:
		}
		e.drain()
	}
}

func (e *Engine) drain() {
	for round := 0; round < maxDrainRounds; round++ {
		due, err := e.store.DueDeliveries(drainBatch)
		if err != nil {
			e.log.Error("读取待投递记录失败", "err", err)
			return
		}
		if len(due) == 0 {
			return
		}
		for _, d := range due {
			e.attempt(d)
		}
		// 失败的记录已经被推到未来，不会再次出现在下一批里，所以循环会收敛。
		if len(due) < drainBatch {
			return
		}
	}
	e.log.Warn("单轮投递达到上限，剩余记录留到下一轮", "rounds", maxDrainRounds)
}

// Inbound 是一次入站事件。
type Inbound struct {
	Source      *store.Source
	Payload     []byte
	Parsed      any
	Headers     map[string]string
	Hops        []string
	TraceID     string
	ContentType string
}

// Submit 把入站事件按规则分发，返回新建的投递条数。
func (e *Engine) Submit(in *Inbound) (int, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return 0, err
	}
	rules, err := e.store.RulesForSource(in.Source.ID)
	if err != nil {
		return 0, err
	}

	data := &TemplateData{
		Payload: in.Parsed,
		Raw:     string(in.Payload),
		Source:  SourceView(in.Source),
		Headers: in.Headers,
		TraceID: in.TraceID,
		Now:     time.Now(),
	}
	hopChain := strings.Join(append(append([]string{}, in.Hops...), in.Source.Slug), ",")

	created := 0
	for _, rule := range rules {
		if !Match(rule.Filters, in.Parsed) {
			continue
		}
		for _, toID := range rule.ToSourceIDs {
			out, err := e.store.GetSource(toID)
			if err != nil {
				e.log.Warn("规则的目标源不存在，跳过", "规则", rule.Name, "源ID", toID)
				continue
			}
			if !out.Enabled || !out.CanSend() {
				e.log.Warn("规则的目标源未启用或不能发送，跳过", "规则", rule.Name, "源", out.Name)
				continue
			}

			body, err := RenderBody(rule.BodyTemplate, data)
			if err != nil {
				e.log.Error("渲染报文模板失败，跳过该目标", "规则", rule.Name, "源", out.Name, "err", err)
				continue
			}
			headers, err := RenderHeaders(rule.HeadersTemplate, data, SourceHeaders(out))
			if err != nil {
				e.log.Error("渲染请求头失败，跳过该目标", "规则", rule.Name, "源", out.Name, "err", err)
				continue
			}
			applyDefaultContentType(headers, rule, in)

			hdrsJSON, err := json.Marshal(headers)
			if err != nil {
				hdrsJSON = []byte("{}")
			}
			subject, err := RenderSubject(rule.SubjectTemplate, data)
			if err != nil {
				e.log.Error("渲染主题模板失败，改用默认主题", "规则", rule.Name, "err", err)
			}
			if subject == "" {
				subject = DefaultSubject(in.Source, data)
			}

			d := &store.Delivery{
				TraceID:     in.TraceID,
				RuleID:      rule.ID,
				InSourceID:  in.Source.ID,
				OutSourceID: out.ID,
				HopChain:    hopChain,
				Status:      store.StatusPending,
				Payload:     Truncate(string(in.Payload), settings.PayloadMaxBytes),
				Rendered:    Truncate(body, settings.PayloadMaxBytes),
				Subject:     subject,
				ReqHeaders:  string(hdrsJSON),
			}
			if err := e.store.CreateDelivery(d); err != nil {
				e.log.Error("写入投递记录失败", "err", err)
				continue
			}
			created++
		}
	}

	if created > 0 {
		e.wake()
	}
	return created, nil
}

// applyDefaultContentType 在用户没指定 Content-Type 时给一个合理的默认值：
// 报文被模板改写过就按 JSON 发，原样透传则沿用入站的类型。
func applyDefaultContentType(headers map[string]string, rule *store.Rule, in *Inbound) {
	if headers["Content-Type"] != "" {
		return
	}
	if strings.TrimSpace(rule.BodyTemplate) != "" {
		headers["Content-Type"] = "application/json"
		return
	}
	if in.ContentType != "" {
		headers["Content-Type"] = in.ContentType
		return
	}
	headers["Content-Type"] = "application/json"
}

// RecordDropped 记一条被拦截的投递（目前只有循环转发会走到这里）。
func (e *Engine) RecordDropped(src *store.Source, traceID, payload, reason, hopChain string) {
	settings, err := e.store.Settings()
	if err != nil {
		e.log.Error("读取设置失败", "err", err)
		return
	}
	d := &store.Delivery{
		TraceID:    traceID,
		InSourceID: src.ID,
		HopChain:   hopChain,
		Status:     store.StatusDropped,
		Payload:    Truncate(payload, settings.PayloadMaxBytes),
		LastError:  reason,
	}
	if err := e.store.CreateDelivery(d); err != nil {
		e.log.Error("记录拦截投递失败", "err", err)
	}
}

// PostTest 构造一条只投给单个目标的测试投递，用于后台的「发送测试」。
func (e *Engine) PostTest(out *store.Source, body string) error {
	d := &store.Delivery{
		TraceID:     "test-" + time.Now().Format("20060102150405"),
		OutSourceID: out.ID,
		Status:      store.StatusPending,
		Payload:     body,
		Rendered:    body,
		Subject:     "[Forward2Any] 测试消息",
		ReqHeaders:  `{"Content-Type":"application/json"}`,
	}
	if err := e.store.CreateDelivery(d); err != nil {
		return err
	}
	e.wake()
	return nil
}

func (e *Engine) attempt(d *store.Delivery) {
	out, err := e.store.GetSource(d.OutSourceID)
	if err != nil {
		// 目标源被删了，这条记录永远不可能成功。
		d.Status = store.StatusDead
		d.LastError = "目标源已不存在"
		d.NextRetryAt = 0
		if err := e.store.UpdateDelivery(d); err != nil {
			e.log.Error("更新投递记录失败", "id", d.ID, "err", err)
		}
		return
	}
	settings, err := e.store.Settings()
	if err != nil {
		e.log.Error("读取设置失败", "err", err)
		return
	}

	d.Attempt++
	var (
		code     int
		respBody string
		sendErr  error
	)
	switch out.Kind {
	case "webhook":
		client, cerr := e.httpClientFor(out, settings)
		if cerr != nil {
			sendErr = cerr
			break
		}
		code, respBody, sendErr = e.sendWebhook(d, out, client)
	case "telegram":
		client, cerr := e.httpClientFor(out, settings)
		if cerr != nil {
			sendErr = cerr
			break
		}
		code, respBody, sendErr = e.sendTelegram(d, out, client)
	case "email":
		sendErr = e.sendMail(d, out)
	default:
		if store.IsChannelKind(out.Kind) {
			client, cerr := e.httpClientFor(out, settings)
			if cerr != nil {
				sendErr = cerr
				break
			}
			code, respBody, sendErr = e.sendChannel(d, out, client)
			break
		}
		sendErr = fmt.Errorf("未知的源类型 %q", out.Kind)
	}

	if sendErr == nil {
		d.Status = store.StatusSuccess
		d.ResponseCode = code
		d.ResponseBody = Truncate(respBody, settings.PayloadMaxBytes)
		d.LastError = ""
		d.NextRetryAt = 0
		e.log.Info("投递成功", "id", d.ID, "规则", d.RuleName, "目标", out.Name, "次数", d.Attempt)
	} else {
		d.ResponseCode = code
		d.ResponseBody = Truncate(respBody, settings.PayloadMaxBytes)
		d.LastError = sendErr.Error()
		if d.Attempt >= settings.RetryMax {
			d.Status = store.StatusDead
			d.NextRetryAt = 0
			e.log.Error("投递最终失败，已放弃", "id", d.ID, "目标", out.Name, "次数", d.Attempt, "err", sendErr)
		} else {
			// 用 failed 而不是退回 pending：日志里能一眼区分
			// 「还没试过」和「试过、失败了、在等下一次」。
			d.Status = store.StatusFailed
			d.NextRetryAt = time.Now().Unix() + backoff(settings.RetryBackoffSeconds, d.Attempt)
			e.log.Warn("投递失败，稍后重试", "id", d.ID, "目标", out.Name,
				"次数", d.Attempt, "下次", d.NextRetryAt, "err", sendErr)
		}
	}

	if err := e.store.UpdateDelivery(d); err != nil {
		e.log.Error("更新投递记录失败", "id", d.ID, "err", err)
	}
}

// Backoff 返回第 attempt 次失败后的等待秒数：指数退避，上限 1 小时。
func backoff(base, attempt int) int64 {
	const maxSec = 3600
	if base < 1 {
		base = 10
	}
	if attempt < 1 {
		attempt = 1
	}
	sec := base
	for i := 1; i < attempt && sec < maxSec; i++ {
		sec *= 2
	}
	if sec > maxSec {
		sec = maxSec
	}
	return int64(sec)
}

// IsLoop 判断本次入站是否已经过本接收源，是则说明转发出环了。
func IsLoop(hops []string, slug string) bool {
	if slug == "" {
		return false
	}
	for _, h := range hops {
		if h == slug {
			return true
		}
	}
	return false
}

// ParseHops 解析 X-F2A-Hops 请求头。
func ParseHops(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
