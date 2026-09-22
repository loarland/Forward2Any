package mailin

import (
	"testing"
	"time"

	"github.com/emersion/go-imap"
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
