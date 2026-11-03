package accountui

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"remote-agents/internal/accounts"
)

type mode int

const (
	browsing mode = iota
	adding
	renaming
	confirming
)

// ActionKind is what the picker asks the box to do.
type ActionKind int

const (
	ActNone ActionKind = iota
	ActQuit
	ActSwitch
	ActAdd
	ActRename
	// ActSave names the box's unsaved login.
	ActSave
	ActRemove
	// ActStart starts a weekly window that has not started.
	ActStart
)

type Action struct {
	Kind    ActionKind
	Name    string
	NewName string
}

// Model is the picker's state: the accounts, the highlighted row, what is
// being typed, and the status line.
type Model struct {
	listing accounts.Listing
	cursor  int
	mode    mode
	input   []rune
	status  string
	failed  bool
}

// New is a picker on listing, its cursor on the active account.
func New(listing accounts.Listing) Model {
	m := Model{listing: listing}
	for i, r := range listing.Accounts {
		if r.Active {
			m.cursor = i
		}
	}
	return m
}

// SetListing shows a new listing, keeping the cursor on the account it was
// on, or on name when that is set.
func (m *Model) SetListing(l accounts.Listing, name string) {
	if name == "" && m.cursor < len(m.listing.Accounts) {
		name = m.listing.Accounts[m.cursor].Name
	}
	m.listing = l
	m.cursor = min(m.cursor, max(len(l.Accounts)-1, 0))
	if i := slices.IndexFunc(l.Accounts, func(r accounts.Row) bool { return r.Name == name }); i >= 0 {
		m.cursor = i
	}
}

// Notify sets the status line; failed marks it as an error.
func (m *Model) Notify(failed bool, format string, args ...any) {
	m.status, m.failed = fmt.Sprintf(format, args...), failed
}

// Row is the account a name names, if the listing has it.
func (m Model) Row(name string) (accounts.Row, bool) {
	i := slices.IndexFunc(m.listing.Accounts, func(r accounts.Row) bool { return r.Name == name })
	if i < 0 {
		return accounts.Row{}, false
	}
	return m.listing.Accounts[i], true
}

func (m Model) current() (accounts.Row, bool) {
	if m.cursor >= len(m.listing.Accounts) {
		return accounts.Row{}, false
	}
	return m.listing.Accounts[m.cursor], true
}

// maxName is the longest name accounts.ValidName admits.
const maxName = 32

// Key handles one key, and returns what the box should do about it.
func (m *Model) Key(k Key) Action {
	switch m.mode {
	case adding, renaming:
		return m.typing(k)
	case confirming:
		return m.confirm(k)
	}
	return m.browse(k)
}

func (m *Model) browse(k Key) Action {
	row, ok := m.current()
	switch {
	case k.Code == KeyEsc || k.Code == KeyCtrlC || k.Code == KeyRune && k.Rune == 'q':
		return Action{Kind: ActQuit}
	case k.Code == KeyUp || k.Code == KeyRune && k.Rune == 'k':
		m.cursor = max(m.cursor-1, 0)
	case k.Code == KeyDown || k.Code == KeyRune && k.Rune == 'j':
		m.cursor = min(m.cursor+1, max(len(m.listing.Accounts)-1, 0))
	case k.Code == KeyRune && k.Rune >= '1' && k.Rune <= '9':
		if n := int(k.Rune - '1'); n < len(m.listing.Accounts) {
			m.cursor = n
		}
	case k.Code == KeyRune && k.Rune == 'a':
		m.mode, m.input, m.status = adding, nil, ""
	case !ok:
	case k.Code == KeyEnter && row.Name == "":
		m.Notify(true, "the box's login has no name yet; press e to name it")
	case k.Code == KeyEnter && row.Active:
		m.Notify(false, "%s is the box's account already", row.Name)
	case k.Code == KeyEnter:
		return Action{Kind: ActSwitch, Name: row.Name}
	case k.Code == KeyRune && k.Rune == 'e':
		m.mode, m.input, m.status = renaming, []rune(row.Name), ""
	case k.Code == KeyRune && k.Rune == 'd' && row.Name == "":
		m.Notify(true, "the box's login has no name, so there is nothing saved to remove")
	case k.Code == KeyRune && k.Rune == 'd':
		m.mode, m.status = confirming, ""
	case k.Code == KeyRune && k.Rune == 's' && row.NotStarted():
		return Action{Kind: ActStart, Name: row.Name}
	}
	return Action{}
}

func (m *Model) typing(k Key) Action {
	switch k.Code {
	case KeyEsc, KeyCtrlC:
		m.mode, m.status = browsing, ""
	case KeyBackspace:
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case KeyRune:
		if len(m.input) < maxName && unicode.IsPrint(k.Rune) {
			m.input = append(m.input, k.Rune)
		}
	case KeyEnter:
		name := string(m.input)
		if err := accounts.ValidName(name); err != nil {
			m.Notify(true, "%v", err)
			return Action{}
		}
		row, _ := m.current()
		mode := m.mode
		m.mode, m.status = browsing, ""
		switch {
		case mode == adding:
			return Action{Kind: ActAdd, Name: name}
		case row.Name == "":
			return Action{Kind: ActSave, Name: name}
		case name != row.Name:
			return Action{Kind: ActRename, Name: row.Name, NewName: name}
		}
	}
	return Action{}
}

func (m *Model) confirm(k Key) Action {
	row, _ := m.current()
	m.mode = browsing
	if k.Code == KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
		return Action{Kind: ActRemove, Name: row.Name}
	}
	m.Notify(false, "kept %s", row.Name)
	return Action{}
}

// View is the picker's lines, each cut to width: the table with its
// cursor, then the prompt or status line, then the keys there are.
func (m Model) View(width int, now time.Time, color bool) []string {
	var lines []string
	if len(m.listing.Accounts) == 0 {
		lines = append(lines, "  no saved accounts yet")
	} else {
		cells := make([][]string, len(m.listing.Accounts))
		for i, r := range m.listing.Accounts {
			cells[i] = accounts.Cells(r, now, color)
		}
		for i, line := range accounts.Align(cells) {
			pointer := "  "
			if i == m.cursor {
				pointer = "▸ "
			}
			lines = append(lines, pointer+line)
		}
	}
	lines = append(lines, "", m.prompt(color), accounts.Paint(accounts.SGRDim, m.footer(), color))
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return lines
}

func (m Model) prompt(color bool) string {
	row, _ := m.current()
	field := string(m.input) + "▏"
	switch {
	case m.mode == adding:
		return "name of the new account: " + field
	case m.mode == renaming && row.Name == "":
		return "name the box's login: " + field
	case m.mode == renaming:
		return "rename " + row.Name + " to: " + field
	case m.mode == confirming:
		return "remove " + row.Name + "? y/N"
	case m.failed:
		return accounts.Paint(accounts.SGRRed, m.status, color)
	}
	return m.status
}

func (m Model) footer() string {
	switch m.mode {
	case adding, renaming:
		return "enter save · esc cancel"
	case confirming:
		return "y remove · any other key keeps it"
	}
	keys := []string{"↑↓ move", "enter switch", "a add", "e rename", "d remove"}
	if row, ok := m.current(); ok && row.NotStarted() {
		keys = append(keys, "s start week")
	}
	return strings.Join(append(keys, "q quit"), " · ")
}
