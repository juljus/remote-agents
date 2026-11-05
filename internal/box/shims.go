// Package box implements the box side of remote-agents: the session launcher,
// shpool glue, the link socket holder and the busybox-style shims.
package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"remote-agents/internal/linkproto"
	"remote-agents/internal/pastefile"
)

// Laptop is what the shims need from the laptop link.
type Laptop interface {
	OpenURL(ctx context.Context, url string) error
	ClipboardTargets(ctx context.Context) ([]string, error)
	ClipboardRead(ctx context.Context, mime string) ([]byte, error)
	ClipboardWrite(ctx context.Context, mime string, data []byte) error
	Hook(ctx context.Context, req linkproto.HookRequest) (linkproto.HookResult, error)
	WriteBack(ctx context.Context, req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error)
	LaptopSave(ctx context.Context, req linkproto.LaptopSaveRequest) (string, error)
}

type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// SessionEnvVar names the shpool session a process runs in, for hook routing.
const SessionEnvVar = "REMOTE_AGENTS_SESSION"

const (
	mimeText = "text/plain"
	mimePNG  = "image/png"
)

var ShimNames = []string{"xclip", "wl-paste", "xdg-open", "cmux", "laptop-save"}

// RunShim runs the shim called name and returns its exit code.
func RunShim(ctx context.Context, name string, args []string, stdio IO, laptop Laptop, getenv func(string) string) int {
	var err error
	switch name {
	case "xclip":
		err = xclip(ctx, args, stdio, laptop)
	case "wl-paste":
		err = wlPaste(ctx, args, stdio, laptop)
	case "xdg-open":
		err = xdgOpen(ctx, args, laptop)
	case "cmux":
		return cmuxHook(ctx, args, stdio, laptop, getenv(SessionEnvVar))
	case "laptop-save":
		err = laptopSave(ctx, args, stdio, laptop, getenv(SessionEnvVar))
	default:
		err = fmt.Errorf("unknown shim %q", name)
	}
	if err != nil {
		fmt.Fprintf(stdio.Stderr, "%s (remote-agents): %v\n", name, err)
		return 1
	}
	return 0
}

// X11 target names that mean plain text.
var textTargets = []string{"UTF8_STRING", "STRING", "TEXT", "text/plain", "text/plain;charset=utf-8"}

func xclip(ctx context.Context, args []string, stdio IO, laptop Laptop) error {
	selection := "primary"
	target := ""
	output := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "-selection", "-sel":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", arg)
			}
			i++
			selection = args[i]
		case "-t", "-target":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", arg)
			}
			i++
			target = args[i]
		case "-o", "-out":
			output = true
		case "-i", "-in":
			output = false
		case "-silent", "-quiet":
		default:
			return fmt.Errorf("unsupported argument %q", arg)
		}
	}
	if selection != "clipboard" && selection != "c" {
		return fmt.Errorf("only the clipboard selection is available, not %q", selection)
	}
	if !output {
		return copyIn(ctx, stdio, laptop, xclipMIME(target))
	}
	if target == "TARGETS" {
		targets, err := laptop.ClipboardTargets(ctx)
		if err != nil {
			return err
		}
		lines := []string{"TARGETS"}
		for _, t := range targets {
			if t == mimeText {
				lines = append(lines, "UTF8_STRING")
			}
			lines = append(lines, t)
		}
		return writeLines(stdio.Stdout, lines)
	}
	return pasteOut(ctx, stdio, laptop, xclipMIME(target), false)
}

func xclipMIME(target string) string {
	if target == "" || slices.Contains(textTargets, target) {
		return mimeText
	}
	return target
}

func wlPaste(ctx context.Context, args []string, stdio IO, laptop Laptop) error {
	mime := mimeText
	list := false
	noNewline := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "-l", "--list-types":
			list = true
		case "-t", "--type":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", arg)
			}
			i++
			mime = xclipMIME(args[i])
		case "-n", "--no-newline":
			noNewline = true
		default:
			return fmt.Errorf("unsupported argument %q", arg)
		}
	}
	if list {
		targets, err := laptop.ClipboardTargets(ctx)
		if err != nil {
			return err
		}
		return writeLines(stdio.Stdout, targets)
	}
	return pasteOut(ctx, stdio, laptop, mime, mime == mimeText && !noNewline)
}

func pasteOut(ctx context.Context, stdio IO, laptop Laptop, mime string, newline bool) error {
	data, err := laptop.ClipboardRead(ctx, mime)
	if err != nil {
		return err
	}
	if newline {
		data = append(data, '\n')
	}
	if _, err := stdio.Stdout.Write(data); err != nil {
		return fmt.Errorf("writing clipboard to stdout: %w", err)
	}
	return nil
}

func copyIn(ctx context.Context, stdio IO, laptop Laptop, mime string) error {
	data, err := io.ReadAll(io.LimitReader(stdio.Stdin, linkproto.MaxMessage/2))
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return laptop.ClipboardWrite(ctx, mime, data)
}

func writeLines(w io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
	}
	return nil
}

func xdgOpen(ctx context.Context, args []string, laptop Laptop) error {
	if len(args) != 1 {
		return errors.New("usage: xdg-open <url>")
	}
	return laptop.OpenURL(ctx, args[0])
}

// cmuxHook relays a cmux hook invocation to the laptop terminal that owns the
// session. Hooks only report status to that terminal, so when it is gone (link
// down, or no terminal attached) the event is dropped with a note instead of
// failing the agent's hook.
func cmuxHook(ctx context.Context, args []string, stdio IO, laptop Laptop, session string) int {
	if session == "" {
		fmt.Fprintf(stdio.Stderr, "cmux (remote-agents): %s is not set\n", SessionEnvVar)
		return 1
	}
	stdin, err := io.ReadAll(io.LimitReader(stdio.Stdin, linkproto.MaxMessage/2))
	if err != nil {
		fmt.Fprintf(stdio.Stderr, "cmux (remote-agents): reading hook payload: %v\n", err)
		return 1
	}
	res, err := laptop.Hook(ctx, linkproto.HookRequest{Session: session, Argv: args, Stdin: stdin})
	if errors.Is(err, linkproto.ErrLinkDown) || errors.Is(err, linkproto.ErrNoTerminal) {
		fmt.Fprintf(stdio.Stderr, "cmux (remote-agents): hook not delivered: %v\n", err)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stdio.Stderr, "cmux (remote-agents): %v\n", err)
		return 1
	}
	if _, err := stdio.Stdout.Write(res.Stdout); err != nil {
		fmt.Fprintf(stdio.Stderr, "cmux (remote-agents): writing hook output: %v\n", err)
		return 1
	}
	if _, err := stdio.Stderr.Write(res.Stderr); err != nil {
		return 1
	}
	return res.ExitCode
}

const laptopSaveUsage = "usage: laptop-save <file> [<laptop path>]"

const laptopSaveHelp = laptopSaveUsage + `
Saves a box file on the laptop: in ~/Downloads, revealed in Finder, unless a
laptop path says where. That may be in ~/Downloads (relative paths are, and a
dir there takes the file under its name) or a file pasted into this session.
A file there already keeps its name: the save takes a free one, except over a
pasted file, whose old version goes to the Trash.`

// laptopSave asks the laptop to fetch a box file; it never sends the content
// over the link itself.
func laptopSave(ctx context.Context, args []string, stdio IO, laptop Laptop, session string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(stdio.Stdout, laptopSaveHelp)
		return err
	}
	if len(args) < 1 || len(args) > 2 {
		return errors.New(laptopSaveUsage)
	}
	path, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	switch {
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", args[0])
	case info.Size() > pastefile.MaxSize:
		return fmt.Errorf("%s is over %d MB", args[0], pastefile.MaxSize>>20)
	}
	req := linkproto.LaptopSaveRequest{Session: session, BoxPath: path}
	if len(args) == 2 {
		req.Dest = args[1]
	}
	saved, err := laptop.LaptopSave(ctx, req)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdio.Stdout, "saved to %s on the laptop\n", saved)
	return err
}
