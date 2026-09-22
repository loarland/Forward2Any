package mailin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
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
