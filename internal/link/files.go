package link

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"remote-agents/internal/laptopfiles"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/pastefile"
	"remote-agents/internal/sshrun"
)

// Fetcher streams a box file to the laptop.
type Fetcher interface {
	Fetch(ctx context.Context, boxPath string, w io.Writer) error
}

// Revealer shows a laptop file in Finder.
type Revealer interface {
	Reveal(ctx context.Context, path string) error
}

// noticeTimeout bounds handing a notice to the session's wrapper.
const noticeTimeout = 5 * time.Second

func (h *Handler) fetch(ctx context.Context, boxPath string) laptopfiles.Fetch {
	return func(w io.Writer) error { return h.Fetcher.Fetch(ctx, boxPath, w) }
}

// WriteBack takes the box's copy of a file pasted into a session, fetching
// it, onto the laptop as laptopfiles decides; the session's screen hears of
// a conflict or a failure.
func (h *Handler) WriteBack(ctx context.Context, req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
	name := filepath.Base(req.Origin)
	res, err := h.Files.WriteBack(ctx, req.Session, req.Origin, req.BoxPath, h.fetch(ctx, req.BoxPath))
	if err != nil {
		h.notify(ctx, req.Session, fmt.Sprintf("%s not written back: %v", name, err))
		return linkproto.WriteBackResult{}, refusal(err)
	}
	h.Logf("%s: write-back of %s: %s, %s", req.Session, req.BoxPath, res.Outcome, res.Path)
	if res.Outcome == laptopfiles.Conflict {
		h.notify(ctx, req.Session, fmt.Sprintf("%s changed on the laptop since the paste; the box's version is in %s", name, filepath.Base(res.Path)))
	}
	h.reportLeftover(ctx, req.Session, res)
	return linkproto.WriteBackResult{Outcome: string(res.Outcome), Path: res.Path, SHA256: res.Sum}, nil
}

// reportLeftover tells the session what a write left behind on the laptop, if anything.
func (h *Handler) reportLeftover(ctx context.Context, session string, res laptopfiles.Result) {
	if res.Leftover == "" {
		return
	}
	h.notify(ctx, session, fmt.Sprintf("%s is written, but %s stays: %v", filepath.Base(res.Path), res.Leftover, res.LeftoverErr))
}

// LaptopSave fetches a box file to where the user lets it go, and shows it
// in Finder. The session's screen hears of a refusal, which may come from
// a prompt nobody answered.
func (h *Handler) LaptopSave(ctx context.Context, req linkproto.LaptopSaveRequest) (linkproto.LaptopSaveResult, error) {
	res, err := h.Files.Save(ctx, req.Session, req.BoxPath, req.Dest, h.fetch(ctx, req.BoxPath))
	if errors.Is(err, laptopfiles.ErrRefused) && req.Session != "" {
		h.notify(ctx, req.Session, fmt.Sprintf("%s not saved: %v", filepath.Base(req.BoxPath), err))
	}
	if err != nil {
		return linkproto.LaptopSaveResult{}, refusal(err)
	}
	h.Logf("saved %s from the box to %s", req.BoxPath, res.Path)
	h.reportLeftover(ctx, req.Session, res)
	// The file is saved; showing it is a courtesy whose failure the box need not hear of.
	if err := h.Revealer.Reveal(ctx, res.Path); err != nil {
		h.Logf("showing %s in Finder: %v", res.Path, err)
	}
	return linkproto.LaptopSaveResult{Path: res.Path, SHA256: res.Sum, Linked: res.Linked}, nil
}

// refusal marks a laptopfiles refusal as one for the box, which then stops asking.
func refusal(err error) error {
	if errors.Is(err, laptopfiles.ErrRefused) {
		return linkproto.Refusal(err)
	}
	return err
}

// notify shows text on the screen of the wrapper showing session, if one does.
func (h *Handler) notify(ctx context.Context, session, text string) {
	h.Logf("%s: notice: %s", session, text)
	caller, err := h.Registry.Lookup(session)
	if err != nil {
		h.Logf("%s: notice not shown: %v", session, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout)
	defer cancel()
	resp, err := caller.Call(ctx, linkproto.Request{Notice: &linkproto.NoticeRequest{Session: session, Text: text}})
	if err == nil {
		err = resp.Err()
	}
	if err != nil {
		h.Logf("%s: notice not shown: %v", session, err)
	}
}

// SSHFetcher fetches box files with send-file over an ssh of their own, so a
// file of any size the box sends leaves the link free.
type SSHFetcher struct {
	SSH string
	Box sshrun.Box
}

func (f SSHFetcher) Fetch(ctx context.Context, boxPath string, w io.Writer) error {
	var ask bytes.Buffer
	if err := pastefile.WriteFrame(&ask, []byte(boxPath)); err != nil {
		return err
	}
	out, sent := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := readOneFile(out, w)
		// ssh must not block on output nobody reads.
		out.CloseWithError(errors.Join(err, errors.New("the fetch stopped reading")))
		copied <- err
	}()
	var stderr bytes.Buffer
	res := sshrun.Run(ctx, sshrun.SendFileArgv(f.SSH, f.Box), sshrun.Stdio{Stdin: &ask, Stdout: sent, Stderr: &stderr}, 2*time.Second)
	sent.Close()
	copyErr := <-copied
	switch {
	case res.Err != nil && copyErr != nil:
		return copyErr
	case res.Err != nil:
		return res.Err
	case res.Transport:
		return fmt.Errorf("fetching from %s: %s", f.Box.Host, res.Detail)
	case res.ExitCode != 0:
		return fmt.Errorf("box: %s", strings.TrimPrefix(strings.TrimSpace(stderr.String()), "remote-agents-box: "))
	}
	return copyErr
}

// readOneFile copies the one file frame r holds to w.
func readOneFile(r io.Reader, w io.Writer) error {
	frames := pastefile.NewReader(r)
	if err := frames.CopyNext(w); errors.Is(err, io.EOF) {
		return errors.New("the box sent no file")
	} else if err != nil {
		return err
	}
	if _, err := frames.Next(); !errors.Is(err, io.EOF) {
		return errors.Join(errors.New("the box sent more than the file"), err)
	}
	return nil
}

// CommandRevealer shows a file by running Argv with its path after them.
type CommandRevealer struct {
	Argv []string
}

func (r CommandRevealer) Reveal(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, r.Argv[0], append(r.Argv[1:], path)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", strings.Join(r.Argv, " "), path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
