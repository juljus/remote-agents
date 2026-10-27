package link

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandOpener opens a URL by running Path with the URL as its only argument.
type CommandOpener struct {
	Path string
}

func (o CommandOpener) Open(ctx context.Context, url string) error {
	out, err := exec.CommandContext(ctx, o.Path, url).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", o.Path, url, err, strings.TrimSpace(string(out)))
	}
	return nil
}
