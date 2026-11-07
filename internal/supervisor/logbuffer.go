package supervisor

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	logCapacity = 2000
	maxLineLen  = 16 * 1024
)

// logBuffer keeps the last lines written by a project (build and runtime output)
// and mirrors them to the container's stdout, prefixed with the project name.
type logBuffer struct {
	mu      sync.Mutex
	prefix  string
	mirror  io.Writer
	lines   []string
	next    int
	full    bool
	partial []byte
}

func newLogBuffer(prefix string, mirror io.Writer) *logBuffer {
	return &logBuffer{prefix: prefix, mirror: mirror, lines: make([]string, logCapacity)}
}

// Write implements io.Writer, splitting the stream into lines.
func (b *logBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, data...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.addLocked(string(b.partial[:i]))
		b.partial = b.partial[i+1:]
	}
	if len(b.partial) > maxLineLen {
		b.addLocked(string(b.partial))
		b.partial = nil
	}
	if len(b.partial) == 0 {
		b.partial = nil
	}
	return len(data), nil
}

// system logs a message emitted by self-hoster itself rather than by the project.
func (b *logBuffer) system(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addLocked("[self-hoster] " + fmt.Sprintf(format, args...))
}

// Lines returns the buffered lines, oldest first.
func (b *logBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		out := make([]string, b.next)
		copy(out, b.lines[:b.next])
		return out
	}
	out := make([]string, 0, logCapacity)
	out = append(out, b.lines[b.next:]...)
	return append(out, b.lines[:b.next]...)
}

func (b *logBuffer) addLocked(line string) {
	line = strings.TrimRight(line, "\r")
	b.lines[b.next] = time.Now().Format("15:04:05") + " " + line
	b.next++
	if b.next == logCapacity {
		b.next = 0
		b.full = true
	}
	if b.mirror != nil {
		fmt.Fprintf(b.mirror, "[%s] %s\n", b.prefix, line)
	}
}
