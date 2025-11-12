package ui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type Loader struct {
	msg      string
	interval time.Duration
	done     chan struct{}
	wg       sync.WaitGroup
	enabled  bool
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	// Char device 基本可视作 TTY；Windows/WSL 下同样适用。
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func Start(message string, interval time.Duration) *Loader {
	l := &Loader{
		msg:      message,
		interval: interval,
		done:     make(chan struct{}),
		enabled:  isTerminal(),
	}
	if !l.enabled {
		return l
	}
	frames := []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(l.interval)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-l.done:
				// 清一行
				fmt.Fprint(os.Stdout, "\r\033[2K")
				return
			case <-t.C:
				// \r 回到行首；\033[2K 清除整行（在大多数终端可用）
				fmt.Fprintf(os.Stdout, "\r\033[2K⏳ %s %c", l.msg, frames[i%len(frames)])
				i++
			}
		}
	}()
	return l
}

func (l *Loader) TickHint(hint string) {
	if !l.enabled {
		return
	}
	fmt.Fprintf(os.Stdout, "\r\033[2K⏳ %s — %s", l.msg, hint)
}

func (l *Loader) Stop(finalLine string) {
	if !l.enabled {
		return
	}
	select {
	case <-l.done:
		// 已关闭
	default:
		close(l.done)
	}
	l.wg.Wait()
	// 输出一行最终结果（可留空）
	if finalLine != "" {
		fmt.Fprintf(os.Stdout, "\r\033[2K%s\n", finalLine)
	} else {
		// 保证换行，避免与后续日志黏连
		fmt.Fprintln(os.Stdout)
	}
}
