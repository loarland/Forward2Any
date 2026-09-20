// Package mailin 通过 IMAP 轮询接收邮件，规范化成统一载荷后交给转发引擎。
//
// 每个「邮件接收源」一条独立协程；配置变化时由本包自行比对数据库并重建，
// 调用方只需要在改完源之后喊一声 Reload。
package mailin

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	gomail "github.com/emersion/go-message/mail"

	"github.com/loarland/Forward2Any/internal/engine"
	"github.com/loarland/Forward2Any/internal/store"
)

const (
	dialTimeout    = 15 * time.Second
	cycleBudget    = 60 * time.Second // 单轮操作的总时限，防止连接卡死
	reconcileEvery = 30 * time.Second
	maxBodyBytes   = 256 << 10
	maxPerCycle    = 50 // 单轮最多处理多少封，避免积压邮件一次全灌进来
)

type Poller struct {
	store *store.Store
	eng   *engine.Engine
	log   *slog.Logger

	wake chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	workers map[int64]*worker
	started bool
}

type worker struct {
	cancel    context.CancelFunc
	updatedAt int64
}

func New(st *store.Store, eng *engine.Engine, log *slog.Logger) *Poller {
	return &Poller{
		store:   st,
		eng:     eng,
		log:     log,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		workers: make(map[int64]*worker),
	}
}

func (p *Poller) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()

	p.wg.Add(1)
	go p.supervise()
}

func (p *Poller) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	p.started = false
	p.mu.Unlock()

	close(p.stop)
	p.wg.Wait()
}

// Reload 让巡检立刻重新比对一次，不必等下一个周期。
func (p *Poller) Reload() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Poller) supervise() {
	defer p.wg.Done()

	p.reconcile()
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			p.stopAll()
			return
		case <-p.wake:
		case <-ticker.C:
		}
		p.reconcile()
	}
}

func (p *Poller) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, w := range p.workers {
		w.cancel()
		delete(p.workers, id)
	}
}

// reconcile 让运行中的轮询协程与数据库里的邮件接收源保持一致。
// 源的配置改了（updated_at 变了）也会重启，好让新的间隔/密码生效。
func (p *Poller) reconcile() {
	sources, err := p.store.MailReceivingSources()
	if err != nil {
		p.log.Error("读取邮件接收源失败", "err", err)
		return
	}
	want := make(map[int64]*store.Source, len(sources))
	for _, src := range sources {
		want[src.ID] = src
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for id, w := range p.workers {
		src, keep := want[id]
		if keep && src.UpdatedAt == w.updatedAt {
			continue
		}
		w.cancel()
		delete(p.workers, id)
		if keep {
			p.log.Info("邮件接收源配置已变化，重启轮询", "源", src.Name)
		} else {
			p.log.Info("停止邮件轮询", "源ID", id)
		}
	}

	for id, src := range want {
		if _, ok := p.workers[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.workers[id] = &worker{cancel: cancel, updatedAt: src.UpdatedAt}
		p.wg.Add(1)
		go func(src *store.Source) {
			defer p.wg.Done()
			p.poll(ctx, src)
		}(src)
		p.log.Info("启动邮件轮询", "源", src.Name, "邮箱", src.IMAPFolder, "间隔秒", src.IMAPInterval)
	}
}

func (p *Poller) poll(ctx context.Context, src *store.Source) {
	interval := time.Duration(src.IMAPInterval) * time.Second
	if interval < 10*time.Second {
		interval = 60 * time.Second
	}

	// 先等一个（带抖动的）间隔再开工：多个源同时连同一台邮件服务器会被限流。
	timer := time.NewTimer(jitter(interval))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := p.cycle(ctx, src); err != nil {
			p.log.Warn("邮件轮询失败", "源", src.Name, "err", err)
		}
		timer.Reset(jitter(interval))
	}
}

// jitter 在间隔上叠加最多 25% 的随机抖动。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(d/4)+1))
	if err != nil {
		return d
	}
	return d + time.Duration(n.Int64())
}

func (p *Poller) cycle(ctx context.Context, src *store.Source) error {
	c, conn, err := dial(src)
	if err != nil {
		return err
	}
	defer func() {
		_ = c.Logout()
		_ = conn.Close()
	}()

	if _, err := c.Select(src.IMAPFolder, false); err != nil {
		return fmt.Errorf("打开邮箱 %s: %w", src.IMAPFolder, err)
	}

	ids, err := c.Search(&imap.SearchCriteria{WithoutFlags: []string{imap.SeenFlag}})
	if err != nil {
		return fmt.Errorf("搜索未读邮件: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxPerCycle {
		ids = ids[:maxPerCycle]
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, section.FetchItem()}
	ch := make(chan *imap.Message, 10)

	// 给整轮操作一个总时限：go-imap v1 没有 context 支持，只能靠连接超时兜底。
	_ = conn.SetDeadline(time.Now().Add(cycleBudget))

	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, items, ch) }()

	var handled []uint32
	for msg := range ch {
		if ctx.Err() != nil {
			break
		}
		raw := msg.GetBody(section)
		if raw == nil {
			continue
		}
		n, err := normalize(raw, src)
		if err != nil {
			p.log.Warn("解析邮件失败，跳过", "源", src.Name, "seq", msg.SeqNum, "err", err)
			continue
		}

		if engine.IsLoop(n.Hops, src.Slug) {
			p.log.Warn("检测到循环邮件，已拦截", "源", src.Name, "跳链", strings.Join(n.Hops, ","))
			p.eng.RecordDropped(src, n.TraceID, string(n.Payload), "检测到循环转发，已拦截", strings.Join(n.Hops, ","))
			handled = append(handled, msg.SeqNum)
			continue
		}

		var parsed any
		_ = json.Unmarshal(n.Payload, &parsed)
		if _, err := p.eng.Submit(&engine.Inbound{
			Source:      src,
			Payload:     n.Payload,
			Parsed:      parsed,
			Headers:     n.Headers,
			Hops:        n.Hops,
			TraceID:     n.TraceID,
			ContentType: "application/json",
		}); err != nil {
			p.log.Error("提交邮件事件失败", "源", src.Name, "err", err)
			continue
		}
		handled = append(handled, msg.SeqNum)
	}
	if err := <-done; err != nil {
		return fmt.Errorf("拉取邮件: %w", err)
	}

	if len(handled) == 0 {
		return nil
	}
	// 处理成功才标记已读：失败的下轮还会被取到。
	_ = conn.SetDeadline(time.Now().Add(cycleBudget))
	seen := new(imap.SeqSet)
	seen.AddNum(handled...)
	op := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := c.Store(seen, op, []interface{}{imap.SeenFlag}, nil); err != nil {
		return fmt.Errorf("标记已读: %w", err)
	}
	p.log.Info("处理邮件", "源", src.Name, "封数", len(handled))
	return nil
}

func dial(src *store.Source) (*client.Client, net.Conn, error) {
	addr := net.JoinHostPort(src.IMAPHost, strconv.Itoa(src.IMAPPort))
	d := &net.Dialer{Timeout: dialTimeout}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("连接 %s: %w", addr, err)
	}

	if src.IMAPTLS {
		host, _, _ := net.SplitHostPort(addr)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("TLS 握手失败（若服务器用自签证书，请改用非加密端口）: %w", err)
		}
		conn = tlsConn
	}

	c, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("初始化 IMAP 会话: %w", err)
	}
	if err := c.Login(src.IMAPUser, src.IMAPPass); err != nil {
		c.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("IMAP 登录失败: %w", err)
	}
	return c, conn, nil
}

// ---------- 邮件 -> 统一载荷 ----------

type normalizedMail struct {
	Payload []byte
	Headers map[string]string
	Hops    []string
	TraceID string
}

// normalize 把一封 MIME 邮件转成和 webhook 一致的 JSON 结构。
func normalize(raw io.Reader, src *store.Source) (*normalizedMail, error) {
	mr, err := gomail.CreateReader(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 MIME: %w", err)
	}

	out := map[string]any{
		"from": "", "from_name": "",
		"to": []string{}, "cc": []string{},
		"subject": "", "date": "",
		"text": "", "html": "",
		"attachments": []map[string]any{},
	}
	headers := map[string]string{}

	h := mr.Header
	if s, err := h.Subject(); err == nil {
		out["subject"] = s
	}
	if d, err := h.Date(); err == nil {
		out["date"] = d.Format(time.RFC3339)
	}
	if list, err := h.AddressList("From"); err == nil && len(list) > 0 {
		out["from"] = list[0].Address
		out["from_name"] = list[0].Name
	}
	out["to"] = addresses(h, "To")
	out["cc"] = addresses(h, "Cc")

	for _, k := range []string{"X-F2A-Hops", "X-F2A-Trace", "Message-Id", "Reply-To"} {
		if v := h.Get(k); v != "" {
			headers[k] = v
		}
	}

	var text, html strings.Builder
	var attachments []map[string]any

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 MIME 分段: %w", err)
		}

		switch ph := part.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			body, err := io.ReadAll(io.LimitReader(part.Body, maxBodyBytes))
			if err != nil {
				continue
			}
			if strings.EqualFold(ct, "text/html") {
				html.Write(body)
			} else {
				text.Write(body)
			}
		case *gomail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			filename, _ := ph.Filename()
			// 附件内容不转存，只记元信息。
			size, _ := io.Copy(io.Discard, io.LimitReader(part.Body, maxBodyBytes))
			attachments = append(attachments, map[string]any{
				"filename":     filename,
				"content_type": ct,
				"size":         size,
			})
		}
	}
	out["text"] = text.String()
	out["html"] = html.String()
	if attachments != nil {
		out["attachments"] = attachments
	}

	payload, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	traceID := headers["X-F2A-Trace"]
	if traceID == "" {
		traceID, _ = store.RandomHex(8)
	}
	return &normalizedMail{
		Payload: payload,
		Headers: headers,
		Hops:    engine.ParseHops(headers["X-F2A-Hops"]),
		TraceID: traceID,
	}, nil
}

func addresses(h gomail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}
