package handlers

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	defaultLogBufferSize = 64 * 1024
	maxLogLineSize       = 1024 * 1024

	defaultLogTailLines = 100
	// maxLogTailLines caps what one request makes the kubelet read back and
	// this server relay: enough to look back, not to dump a node's disk.
	maxLogTailLines = 10000
)

// tailLinesParam reads the tailLines query parameter: the default when absent
// or not a positive number, capped at maxLogTailLines.
func tailLinesParam(c *gin.Context) int64 {
	tailLines := int64(defaultLogTailLines)
	if tl := c.Query("tailLines"); tl != "" {
		if v, err := strconv.ParseInt(tl, 10, 64); err == nil && v > 0 {
			tailLines = min(v, maxLogTailLines)
		}
	}
	return tailLines
}

func newLogStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, defaultLogBufferSize), maxLogLineSize)
	return scanner
}

func logStreamErrorMessage(err error) string {
	if errors.Is(err, bufio.ErrTooLong) {
		return "log line too long, stream interrupted"
	}
	return "log stream interrupted"
}

// writeLogs answers a log request: the lines of stream as Server-Sent
// Events when follow, plain text otherwise.
func writeLogs(c *gin.Context, stream io.Reader, follow bool) {
	if !follow {
		c.DataFromReader(http.StatusOK, -1, "text/plain; charset=utf-8", stream, nil)
		return
	}

	type logLine struct {
		text string
		err  error
	}
	lines := make(chan logLine)
	// Captured here: gin returns the *gin.Context to its pool as soon as
	// the handler returns and overwrites Request, so reading it from the
	// goroutine races and would watch another request's cancellation.
	streamCtx := c.Request.Context()
	go func() {
		defer close(lines)
		scanner := newLogStreamScanner(stream)
		for scanner.Scan() {
			select {
			case lines <- logLine{text: scanner.Text()}:
			case <-streamCtx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- logLine{err: err}:
			case <-streamCtx.Done():
			}
		}
	}()

	serveStream(c, lines, func(s *sseStream, line logLine) error {
		if line.err != nil {
			slog.Warn("Log stream ended on a scanner error", "error", line.err)
			_ = s.Event("error", logStreamErrorMessage(line.err))
			return line.err
		}
		return s.Event("message", line.text)
	})
}
