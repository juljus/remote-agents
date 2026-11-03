package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"remote-agents/internal/paste"
	"remote-agents/internal/pastefile"
	"remote-agents/internal/reconnect"
	"remote-agents/internal/sshrun"
)

// stallTimeout bounds how long one paste's exchange with the box may go without moving a byte.
var stallTimeout = 15 * time.Second

var errStalled = errors.New("the exchange stalled")

// errBoxEnded is what the exchange reads once ssh has exited; its writes fail
// with EPIPE, or ErrClosed once the pipe is closed under them.
var errBoxEnded = errors.New("the box ended the exchange early")

// pasteRewriter is the Rewrite of an attempt's paste filter: each laptop path
// in a paste that the box lacks becomes the box path of its uploaded copy.
// A path the box has stays as typed, so the agent works on the box's own
// file. A file that cannot upload stays as typed too, and the user is told
// why; when the exchange fails, the whole paste goes on unchanged.
func pasteRewriter(ctx context.Context, opts Options, out *screenOut) func([]byte) []byte {
	return func(content []byte) []byte {
		files, err := paste.Files(content)
		if err == nil && len(files) == 0 {
			return content
		}
		var up uploaded
		if err == nil {
			up, err = upload(ctx, opts, files)
		}
		if ctx.Err() != nil {
			opts.Logf("%s: the attempt ended while a pasted file was uploading; the paste went with it", opts.Spec.Session)
			return content
		}
		if err != nil {
			opts.Logf("%s: pasted files not uploaded, their laptop paths went in as text: %v", opts.Spec.Session, err)
			out.notify("pasted files not uploaded: " + err.Error())
			return content
		}
		if len(up.refused) > 0 {
			reasons := errors.Join(up.refused...)
			opts.Logf("%s: pasted paths not uploaded, they went in as typed: %v", opts.Spec.Session, reasons)
			out.notify("not uploaded: " + strings.ReplaceAll(reasons.Error(), "\n", "; "))
		}
		return paste.Replace(content, up.sent, up.boxPaths)
	}
}

// uploaded is what a paste-files exchange made of a paste's laptop files:
// boxPaths[i] is where sent[i] landed, and refused says why each file the
// box lacks did not go.
type uploaded struct {
	sent     []paste.File
	boxPaths []string
	refused  []error
}

// upload runs one paste-files exchange with the box over ssh.
func upload(ctx context.Context, opts Options, files []paste.File) (uploaded, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer stall.Stop()
	// ssh reads its stdin straight from the pipe, so its exit is not held up by a copy waiting on the laptop.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return uploaded{}, err
	}
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	ran := make(chan reconnect.Result, 1)
	go func() {
		res := sshrun.Run(ctx, sshrun.PasteFilesArgv(opts.SSH, opts.Box), sshrun.Stdio{Stdin: stdinR, Stdout: stdoutW, Stderr: &stderr}, 2*time.Second)
		// A process ssh left behind may still hold the pipes; the exchange is over all the same.
		stdoutW.CloseWithError(errBoxEnded)
		stdinR.Close()
		stdinW.Close()
		ran <- res
	}()
	up, err := exchange(movingWriter{stdinW, stall}, movingReader{stdoutR, stall}, files)
	if err != nil {
		cancel(nil)
	}
	// ssh's exit waits on its output reaching a reader.
	stdoutR.Close()
	res := <-ran
	boxEnded := errors.Is(err, errBoxEnded) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
	switch {
	case err != nil && !boxEnded:
		return uploaded{}, err
	case res.Err != nil:
		return uploaded{}, res.Err
	case errors.Is(context.Cause(ctx), errStalled):
		return uploaded{}, fmt.Errorf("the upload to %s stalled for %s", opts.Box.Host, stallTimeout)
	case res.Transport:
		return uploaded{}, fmt.Errorf("uploading to %s: %s", opts.Box.Host, res.Detail)
	case res.ExitCode != 0:
		return uploaded{}, fmt.Errorf("box: %s", strings.TrimPrefix(strings.TrimSpace(stderr.String()), "remote-agents-box: "))
	case err != nil:
		return uploaded{}, err
	}
	return up, nil
}

// exchange is the laptop's side of paste-files: it asks the box which of
// files it has, sends those it lacks that can go, ends its side and reads
// where they landed. The box's progress, read while the files go, keeps the
// stall timer off.
func exchange(w io.WriteCloser, r io.Reader, files []paste.File) (uploaded, error) {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	if err := pastefile.WriteQuery(w, paths); err != nil {
		return uploaded{}, fmt.Errorf("asking the box: %w", err)
	}
	frames := pastefile.NewReader(r)
	onBox, err := frames.Answer(len(files))
	if err != nil {
		return uploaded{}, err
	}
	var up uploaded
	var srcs []*os.File
	defer func() {
		for _, src := range srcs {
			// Closing a file opened only for reading loses nothing.
			_ = src.Close()
		}
	}()
	for i, f := range files {
		if onBox[i] {
			continue
		}
		if err := f.Uploadable(); err != nil {
			up.refused = append(up.refused, err)
			continue
		}
		src, err := f.Open()
		if err != nil {
			up.refused = append(up.refused, err)
			continue
		}
		up.sent = append(up.sent, f)
		srcs = append(srcs, src)
	}
	stored := make(chan storedResult, 1)
	go func() {
		paths, err := readStored(frames, len(up.sent))
		stored <- storedResult{paths, err}
	}()
	for i, f := range up.sent {
		if err := send(w, f, srcs[i]); err != nil {
			return uploaded{}, err
		}
	}
	if err := w.Close(); err != nil {
		return uploaded{}, err
	}
	res := <-stored
	if res.err != nil {
		return uploaded{}, res.err
	}
	up.boxPaths = res.paths
	return up, nil
}

type storedResult struct {
	paths []string
	err   error
}

// readStored reads the paths of n stored files, passing over the progress frames before them.
func readStored(frames *pastefile.Reader, n int) ([]string, error) {
	var paths []string
	for len(paths) < n {
		path, err := frames.Next()
		if err != nil {
			return nil, fmt.Errorf("reading where the uploads landed: %w", err)
		}
		if len(path) == 0 {
			continue
		}
		if !strings.HasPrefix(string(path), "/") {
			return nil, fmt.Errorf("the box stored an upload at %q", path)
		}
		paths = append(paths, string(path))
	}
	return paths, nil
}

// send writes f's base name and its content, read from src, to the box.
func send(w io.Writer, f paste.File, src *os.File) error {
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if err := pastefile.WriteFrame(w, []byte(filepath.Base(f.Path))); err != nil {
		return fmt.Errorf("uploading %s: %w", f.Path, err)
	}
	if err := pastefile.CopyFrame(w, src, info.Size()); err != nil {
		return fmt.Errorf("uploading %s: %w", f.Path, err)
	}
	return nil
}

// movingWriter and movingReader hold off the stall timer while bytes move through them.
type movingWriter struct {
	w     io.WriteCloser
	stall *time.Timer
}

func (m movingWriter) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if n > 0 {
		m.stall.Reset(stallTimeout)
	}
	return n, err
}

func (m movingWriter) Close() error { return m.w.Close() }

type movingReader struct {
	r     io.Reader
	stall *time.Timer
}

func (m movingReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.stall.Reset(stallTimeout)
	}
	return n, err
}

// noticeQuiet is how long the agent's output must pause before a notice is drawn.
const noticeQuiet = 150 * time.Millisecond

// noticeWait bounds the wait for that pause; a notice that finds none is dropped.
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
	stopOnce  sync.Once
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
			quiet := time.Since(s.lastWrite) >= noticeQuiet
			if quiet {
				s.draw(text)
			}
			s.mu.Unlock()
			if quiet {
				return
			}
			// Drawing into a flow of output could split an escape sequence.
			if time.Now().After(deadline) {
				s.logf("notice not shown, the output never paused: %s", text)
				return
			}
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

// close drops the notices not drawn yet; later calls do nothing.
func (s *screenOut) close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.notices.Wait()
}
