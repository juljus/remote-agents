package accounts

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Choose asks which account to switch to, by its number in Render's table
// or by name. It returns "" when the answer keeps the active account.
func Choose(in io.Reader, out io.Writer, l Listing) (string, error) {
	named := l.Named()
	if len(named) == 0 {
		return "", errors.New("no saved accounts; add one with `rclaude account add <name>`")
	}
	keep := "keeps the current login"
	if active := l.Active(); active != "" {
		keep = "keeps " + active
	}
	fmt.Fprintf(out, "Switch to (1-%d or a name, Enter %s): ", len(named), keep)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("reading the choice: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return "", nil
	}
	if n, err := strconv.Atoi(answer); err == nil {
		if n < 1 || n > len(named) {
			return "", fmt.Errorf("no account %d", n)
		}
		answer = named[n-1].Name
	}
	for _, r := range named {
		if r.Name == answer {
			if r.Active {
				return "", nil
			}
			return answer, nil
		}
	}
	return "", fmt.Errorf("no account %s", answer)
}
