package accounts

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Listing is what `account list` reports, as JSON between box and laptop.
type Listing struct {
	Accounts []Row `json:"accounts"`
}

// Row is one account. Name is "" for a live login no entry names.
type Row struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Active bool   `json:"active"`
	// Weekly is nil for a plan without a weekly limit, and when Error is set.
	Weekly *Window `json:"weekly"`
	Error  string  `json:"error,omitempty"`
}

// Window is the weekly usage window.
type Window struct {
	Utilization float64 `json:"utilization"`
	// ResetsAt is nil while the window has not started.
	ResetsAt *time.Time `json:"resets_at"`
}

// NotStarted reports a weekly window that starts with the account's next request.
func (r Row) NotStarted() bool { return r.Error == "" && r.Weekly != nil && r.Weekly.ResetsAt == nil }

// pendingRow is a row whose usage is still to be read with login, if set.
type pendingRow[L any] struct {
	row   Row
	login *L
}

// readUsage reads each pending row's usage at once, and lists the rows;
// a read that fails becomes its row's error.
func readUsage[L any](rows []pendingRow[L], read func(L) (*Window, error)) Listing {
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].login == nil {
			continue
		}
		wg.Go(func() {
			weekly, err := read(*rows[i].login)
			if err != nil {
				rows[i].row.Error = err.Error()
				return
			}
			rows[i].row.Weekly = weekly
		})
	}
	wg.Wait()
	out := Listing{Accounts: make([]Row, len(rows))}
	for i, r := range rows {
		out.Accounts[i] = r.row
	}
	return out
}

// Render writes the listing as a table that marks the active account;
// times are in now's location, colour is for a terminal, and cli is the
// command that saves accounts.
func Render(w io.Writer, l Listing, now time.Time, color bool, cli string) error {
	cells := make([][]string, len(l.Accounts))
	for i, r := range l.Accounts {
		cells[i] = Cells(r, now, color)
	}
	for _, line := range Align(cells) {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	for _, r := range l.Accounts {
		if r.Name == "" && r.Error == "" {
			_, err := fmt.Fprintf(w, "The box's login has no name: keep it with `%s account save <name>`.\n", cli)
			return err
		}
	}
	return nil
}
