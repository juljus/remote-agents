package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"remote-agents/internal/paste"
	"remote-agents/internal/pastefile"
	"remote-agents/internal/sshrun"
)

// uploadTimeout bounds the upload of one paste's images.
const uploadTimeout = time.Minute

// pasteRewriter is the Rewrite of an attempt's paste filter: the laptop image
// paths in a paste become the box paths of their uploaded copies. When the
// upload fails, the paste goes on unchanged and the user is told why.
func pasteRewriter(ctx context.Context, opts Options, out *screenOut) func([]byte) []byte {
	return func(content []byte) []byte {
		images, err := paste.Images(content)
		if err == nil && len(images) == 0 {
			return content
		}
		var boxPaths []string
		if err == nil {
			boxPaths, err = upload(ctx, opts, images)
		}
		if err != nil {
			opts.Logf("%s: pasted image not uploaded, its laptop path went in as text: %v", opts.Spec.Session, err)
			out.notify("pasted image not uploaded: " + err.Error())
			return content
		}
		return paste.Replace(content, images, boxPaths)
	}
}

// upload sends the images to the box over one ssh command and returns where they landed.
func upload(ctx context.Context, opts Options, images []paste.Image) ([]string, error) {
	var stream bytes.Buffer
	for _, im := range images {
		data, err := os.ReadFile(im.Path)
		if err != nil {
			return nil, err
		}
		if err := pastefile.WriteFrame(&stream, data); err != nil {
			return nil, fmt.Errorf("%s: %w", im.Path, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	res := sshrun.Run(ctx, sshrun.PasteArgv(opts.SSH, opts.Box), sshrun.Stdio{Stdin: &stream, Stdout: &stdout, Stderr: &stderr}, 2*time.Second)
	switch {
	case res.Err != nil:
		return nil, res.Err
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("the upload to %s took over %s", opts.Box.Host, uploadTimeout)
	case res.Transport:
		return nil, fmt.Errorf("uploading to %s: %s", opts.Box.Host, res.Detail)
	case res.ExitCode != 0:
		return nil, fmt.Errorf("box: %s", strings.TrimPrefix(strings.TrimSpace(stderr.String()), "remote-agents-box: "))
	}
	paths := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(paths) != len(images) {
		return nil, fmt.Errorf("the box stored %d of %d images: %q", len(paths), len(images), stdout.String())
	}
	return paths, nil
}

// noticeQuiet is how long the agent's output must pause before a notice is drawn.
const noticeQuiet = 150 * time.Millisecond

// noticeWait bounds the wait for that pause.
const noticeWait = 2 * time.Second

// screenOut is the one writer of the session's output to the terminal. It
// clears the outage status before the first byte of a redraw, and draws
// notices once the output pauses.
type screenOut struct {
	w      io.Writer
	outage *outage
	width  func() (int, error)
	logf   func(format string, args ...any)

	mu        sync.Mutex
	lastWrite time.Time
	stop      chan struct{}
	notices   sync.WaitGroup
}

func newScreenOut(w io.Writer, o *outage, width func() (int, error), logf func(format string, args ...any)) *screenOut {
	return &screenOut{w: w, outage: o, width: width, logf: logf, stop: make(chan struct{})}
}

func (s *screenOut) Write(p []byte) (int, error) {
	s.outage.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWrite = time.Now()
	return s.w.Write(p)
}

// notify shows text on the bottom row once the agent has redrawn the screen
// after the input that caused it. It stays until the agent draws over it.
func (s *screenOut) notify(text string) {
	s.notices.Add(1)
	go func() {
		defer s.notices.Done()
		deadline := time.Now().Add(noticeWait)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(noticeQuiet / 3):
			}
			s.mu.Lock()
			if time.Since(s.lastWrite) >= noticeQuiet || time.Now().After(deadline) {
				s.draw(text)
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
	}()
}

// draw writes the notice over the bottom row and puts the cursor back.
func (s *screenOut) draw(text string) {
	width, err := s.width()
	if err != nil {
		s.logf("notice: reading terminal width: %v", err)
		return
	}
	line := "[remote-agents] " + printable(text)
	if len(line) > width-1 {
		line = line[:max(width-1, 0)]
	}
	if _, err := fmt.Fprint(s.w, "\x1b7\x1b[999;1H\x1b[2K"+line+"\x1b8"); err != nil {
		s.logf("notice: %v", err)
	}
}

// close drops the notices not drawn yet.
func (s *screenOut) close() {
	close(s.stop)
	s.notices.Wait()
}
