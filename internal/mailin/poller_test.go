package mailin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"

	"github.com/loarland/Forward2Any/internal/engine"
	"github.com/loarland/Forward2Any/internal/store"
)

// 取消轮询之后必须有人继续排空 Fetch 的通道：go-imap v1 的 Fetch 是同步往通道里投递的，
// 消费者一走，它就会永久阻塞在发送上，取邮件那条协程再也回不来，
// Poller.Stop() 里的 wg.Wait() 跟着一起卡死。
func TestDrainInBackgroundUnblocksSender(t *testing.T) {
	ch := make(chan *imap.Message, 1)
	ch <- &imap.Message{} // 先把缓冲占满

	drainInBackground(ch)

	sent := make(chan struct{})
	go func() {
		ch <- &imap.Message{}
		close(ch)
		close(sent)
	}()

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("排空协程没有消费通道，发送方仍然被阻塞")
	}
}

// cycle 走的是 UID 搜索 / UID 拉取 / UID 标记已读。这里起一个内存 IMAP 服务端，
// 塞一封未读邮件进去，验证这一轮确实取到了邮件、并且按 UID 标记了已读。
func TestCycleFetchesUnreadMailByUID(t *testing.T) {
	srv := server.New(memory.New())
	srv.AllowInsecureAuth = true // 测试用明文 LOGIN，不走 TLS
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	const raw = "From: someone@example.org\r\n" +
		"To: inbox@example.org\r\n" +
		"Subject: 一封未读邮件\r\n" +
		"Date: Wed, 11 May 2016 14:31:59 +0000\r\n" +
		"Message-ID: <unread@example.org>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"hello"

	appendUnread := func() {
		c, err := imapclient.Dial(ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Logout()
		if err := c.Login("username", "password"); err != nil {
			t.Fatal(err)
		}
		if err := c.Append("INBOX", nil, time.Now(), bytes.NewReader([]byte(raw))); err != nil {
			t.Fatal(err)
		}
	}
	appendUnread()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	src := &store.Source{
		Name: "邮件接收", Kind: "email", Usage: "in", Enabled: true, Slug: "mail1",
		IMAPHost: host, IMAPPort: port, IMAPUser: "username", IMAPPass: "password",
		IMAPFolder: "INBOX", IMAPInterval: 60,
	}
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(st, engine.New(st, discard), discard)

	if err := p.cycle(context.Background(), src); err != nil {
		t.Fatalf("拉取一轮失败: %v", err)
	}

	// 再连一次确认：没有未读邮件了，说明 UID 拉取与 UID 标记已读都落到了正确的邮件上。
	c, err := imapclient.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	unread, err := c.UidSearch(&imap.SearchCriteria{WithoutFlags: []string{imap.SeenFlag}})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 0 {
		t.Errorf("处理过的邮件应当已标记为已读，仍有 %d 封未读", len(unread))
	}
}

// PEEK 这条约定要能守住：线上必须真的发 BODY.PEEK[]，不能悄悄退回 BODY[]。
//
// 这里在内存 IMAP 服务端前面串一个 TCP 探针，抄下客户端实际发出的字节，
// 断言的是「发出去的到底是什么」，而不是本地某个常量。
func TestCycleFetchesWithPeek(t *testing.T) {
	srv := server.New(memory.New())
	srv.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	appendMail(t, ln.Addr().String())
	proxyAddr, tap := startTapProxy(t, ln.Addr().String())
	host, port := splitAddr(t, proxyAddr)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	src := mailSource(host, port)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(st, engine.New(st, discard), discard)
	if err := p.cycle(context.Background(), src); err != nil {
		t.Fatalf("拉取一轮失败: %v", err)
	}

	sent := tap.String()
	if !strings.Contains(sent, "BODY.PEEK[]") {
		t.Errorf("取正文必须带 PEEK，实际发出的命令里没有 BODY.PEEK[]：\n%s", sent)
	}
	if strings.Contains(sent, "BODY[]") {
		t.Errorf("不该出现不带 PEEK 的 BODY[]：\n%s", sent)
	}
}

// Peek 取到的正文，服务端回来的是 BODY[]（不带 PEEK），客户端必须还能认出来。
func TestBodySectionStillMatchesPlainResponse(t *testing.T) {
	section := bodySection()
	if got := string(section.FetchItem()); got != "BODY.PEEK[]" {
		t.Fatalf("请求项应当是 BODY.PEEK[]，实际 %q", got)
	}
	// 服务端的响应 section 永远不带 PEEK。
	msg := &imap.Message{Body: map[*imap.BodySectionName]imap.Literal{
		{}: strings.NewReader("hello"),
	}}
	body := msg.GetBody(section)
	if body == nil {
		t.Fatal("Peek 的 section 取不到服务端返回的 BODY[] 正文")
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("正文应为 hello，实际 %q", got)
	}
}

// 命令级超时必须设上：go-imap v1 在 Timeout 为 0 时会清掉连接上已有的 deadline，
// 不设它等于所有命令都没有超时，卡死的邮件服务器能把轮询协程一直挂住。
func TestLoginSetsCommandTimeout(t *testing.T) {
	srv := server.New(memory.New())
	srv.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	host, port := splitAddr(t, ln.Addr().String())
	src := mailSource(host, port)

	conn, err := connect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, err := login(conn, src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()

	if c.Timeout != cycleBudget {
		t.Errorf("命令超时应当设为 %v，实际 %v（为 0 时 go-imap 会清掉所有 deadline）",
			cycleBudget, c.Timeout)
	}
}

// 服务器接受连接后一句话都不回：取消轮询必须能立刻把它从阻塞的读里拽出来，
// 否则 Poller.Stop() 的 wg.Wait() 会一直卡着。
func TestCycleReturnsWhenContextCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 只接受连接、永不回话，模拟卡死的邮件服务器。
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	host, port := splitAddr(t, ln.Addr().String())
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(st, engine.New(st, discard), discard)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.cycle(ctx, mailSource(host, port)) }()

	// 等它连上去并卡在等问候语上，然后取消。
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 cycle 仍未返回：Stop() 会卡死在 wg.Wait() 上")
	}
}

// ---------- 测试辅助 ----------

// tapBuffer 是探针抄下来的字节，可能被读协程和测试主协程同时碰，所以要加锁。
type tapBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tapBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func (t *tapBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

// startTapProxy 在本地起一个转发到 target 的 TCP 代理，并把它收到的
// 「客户端 -> 服务端」方向的字节抄进返回的缓冲区。返回代理自己的地址。
func startTapProxy(t *testing.T, target string) (string, *tapBuffer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	tap := new(tapBuffer)
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() { _, _ = io.Copy(upstream, io.TeeReader(client, tap)) }()
				_, _ = io.Copy(client, upstream)
			}()
		}
	}()
	return ln.Addr().String(), tap
}

// splitAddr 把 "127.0.0.1:1234" 拆成主机和端口号。
func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// mailSource 拼一个指向本地测试服务端的邮件接收源。
func mailSource(host string, port int) *store.Source {
	return &store.Source{
		Name: "邮件接收", Kind: "email", Usage: "in", Enabled: true, Slug: "mail-tap",
		IMAPHost: host, IMAPPort: port, IMAPUser: "username", IMAPPass: "password",
		IMAPFolder: "INBOX", IMAPInterval: 60,
	}
}

// appendMail 往内存服务端塞一封未读邮件。
func appendMail(t *testing.T, addr string) {
	t.Helper()
	c, err := imapclient.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	const raw = "From: someone@example.org\r\n" +
		"To: inbox@example.org\r\n" +
		"Subject: 一封未读邮件\r\n" +
		"Date: Wed, 11 May 2016 14:31:59 +0000\r\n" +
		"Message-ID: <peek@example.org>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"hello"
	if err := c.Append("INBOX", nil, time.Now(), bytes.NewReader([]byte(raw))); err != nil {
		t.Fatal(err)
	}
}
