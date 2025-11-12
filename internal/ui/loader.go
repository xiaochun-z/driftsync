package ui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Loader is a simple terminal spinner for long-running tasks.
// It uses only standard library and works safely even if redirected (no-op when not a TTY).
type Loader struct {
	interval time.Duration
	done     chan struct{}
	wg       sync.WaitGroup
	enabled  bool
}

// isTerminal returns true if stdout is a character device (TTY).
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Start launches the spinner. When not running in a terminal, it becomes a no-op.
func Start(interval time.Duration) *Loader {
	l := &Loader{
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
				// Clear the line on stop
				fmt.Fprint(os.Stdout, "\r\033[2K")
				return
			case <-t.C:
				// \r moves to line start; \033[2K clears the entire line
				fmt.Fprintf(os.Stdout, "\r\033[2K%c", frames[i%len(frames)])
				i++
			}
		}
	}()
	return l
}

// Stop terminates the spinner and clears the line.
func (l *Loader) Stop(finalLine string) {
	if !l.enabled {
		return
	}
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	l.wg.Wait()
	fmt.Fprint(os.Stdout, "\r\033[2K") // final clear
	if finalLine != "" {
		fmt.Fprintf(os.Stdout, "%s\n", finalLine)
	} else {
		fmt.Fprintln(os.Stdout)
	}
}
