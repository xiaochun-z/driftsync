package ui

import (
	"fmt"
	"os"
	"time"

	"github.com/theckman/yacspin"
)

type Loader struct {
	sp      *yacspin.Spinner
	enabled bool
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func Start(interval time.Duration) *Loader {
	l := &Loader{}

	// 非 TTY 时直接 no-op，保持现在的行为
	if !isTerminal() {
		return l
	}

	cfg := yacspin.Config{
		Frequency:       interval,
		Writer:          os.Stdout,
		CharSet:         yacspin.CharSets[11], // 任选一个你喜欢的样式
		Suffix:          " syncing...",
		SuffixAutoColon: true,
		ColorAll:        true,
		SpinnerAtEnd:    true, // 停止时把 spinner 留在行首，然后输出 StopMessage
	}

	sp, err := yacspin.New(cfg)
	if err != nil {
		// 初始化失败就退回静默模式，不影响主逻辑
		return l
	}

	if err := sp.Start(); err != nil {
		return l
	}

	l.sp = sp
	l.enabled = true
	return l
}

// Stop: 停止 spinner，并可选输出一行最终信息
func (l *Loader) Stop(finalLine string) {
	if !l.enabled || l.sp == nil {
		return
	}

	if finalLine != "" {
		// 让最后一帧行显示你传入的文字
		l.sp.StopMessage(finalLine)
	}

		// Stop 会处理好清理/刷新行
	_ = l.sp.Stop()
	l.enabled = false

	if finalLine == "" {
		// 想保留空行（和旧版本行为类似），手动补一行
		fmt.Fprintln(os.Stdout)
	}
}
