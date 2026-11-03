package accounts

import (
	"fmt"
	"io"
	"text/tabwriter"
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
	// A nil window is one that is not running; both are nil when Error is set.
	FiveHour *Window `json:"five_hour"`
	SevenDay *Window `json:"seven_day"`
	Error    string  `json:"error,omitempty"`
}

type Window struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// Active is the name of the active account, or "" when there is none or it has no name.
func (l Listing) Active() string {
	for _, r := range l.Accounts {
		if r.Active {
			return r.Name
		}
	}
	return ""
}

// Named are the rows that can be switched to.
func (l Listing) Named() []Row {
	var out []Row
	for _, r := range l.Accounts {
		if r.Name != "" {
			out = append(out, r)
		}
	}
	return out
}

// Render writes the listing as a table, numbering the accounts that can be
// switched to and marking the active one; times are in now's location.
func Render(w io.Writer, l Listing, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tACCOUNT\tEMAIL\t5-HOUR\tWEEKLY")
	n := 0
	for _, r := range l.Accounts {
		num, name := "", r.Name
		if name == "" {
			name = "(no name)"
		} else {
			n++
			num = fmt.Sprint(n)
		}
		if r.Active {
			num += "*"
		}
		// An error, the last cell, runs on past the column widths.
		usage := "error: " + r.Error
		if r.Error == "" {
			usage = renderWindow(r.FiveHour, now) + "\t" + renderWindow(r.SevenDay, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", num, name, r.Email, usage)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range l.Accounts {
		if r.Name == "" && r.Error == "" {
			_, err := fmt.Fprintln(w, "The box's login has no name: keep it with `rclaude account save <name>`.")
			return err
		}
	}
	return nil
}

func renderWindow(w *Window, now time.Time) string {
	if w == nil {
		return "-"
	}
	s := fmt.Sprintf("%.0f%%", w.Utilization)
	if w.ResetsAt == nil {
		return s
	}
	return s + " until " + renderTime(w.ResetsAt.In(now.Location()), now)
}

// renderTime shows a clock time today, a weekday within a week, else a date.
func renderTime(t, now time.Time) string {
	switch {
	case t.Sub(now) < 24*time.Hour && t.Day() == now.Day():
		return t.Format("15:04")
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}
