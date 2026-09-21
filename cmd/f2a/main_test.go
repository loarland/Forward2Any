package main

import (
	"os"
	"testing"

	"github.com/loarland/Forward2Any/internal/store"
)

// healthcheck 只该「读」数据和探活，绝不能顺手把库建出来。
//
// 真踩过：install.sh 装完用 root 跑了一次 healthcheck，它在数据目录里建了个
// root:root 0600 的 f2a.db，systemd 里的 f2a 用户随即打不开库，服务起不来。
// 返回码在这里不重要（探不探得到活取决于环境），要看的是数据目录有没有被写。
func TestHealthcheckDoesNotCreateDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("F2A_DATA_DIR", dir)
	t.Setenv("F2A_PORT", "16099")

	healthcheck(nil)

	if _, err := os.Stat(store.DBPath(dir)); err == nil {
		t.Errorf("healthcheck 把库建出来了（%s）：跑它的用户可能不是服务用户，库里外的属主会不一致",
			store.DBPath(dir))
	}
	if _, err := os.Stat(store.DBPath(dir) + "-wal"); err == nil {
		t.Error("healthcheck 建出了 -wal 文件，说明它真的打开并写了库")
	}
}
