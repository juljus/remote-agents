package accounts

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The colours of the account table, in a terminal.
const (
	sgrReset  = "\x1b[0m"
	SGRDim    = "\x1b[2m"
	SGRRed    = "\x1b[31m"
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[1;33m"
)

// Paint colours s with sgr when color is set.
func Paint(sgr, s string, color bool) string {
	if !color || s == "" {
		return s
	}
	return sgr + s + sgrReset
}

// barCells is how many cells the weekly bar has.
const barCells = 5

// Cells are a row's columns: its name, its email, what is left of its
// weekly window, and whether it is the active account.
func Cells(r Row, now time.Time, color bool) []string {
	name := r.Name
	if name == "" {
		name = Paint(sgrYellow, "unsaved", color)
	}
	tag := ""
	if r.Active {
		tag = Paint(sgrGreen, "active", color)
	}
	return []string{name, r.Email, weekly(r, now, color), tag}
}

// weekly is the share of the weekly window left, as the status line shows
// it, and when the window resets: always by weekday and time, as it resets
// within a week.
func weekly(r Row, now time.Time, color bool) string {
	switch {
	case r.Error != "":
		return Paint(SGRRed, "error: "+r.Error, color)
	case r.Weekly == nil:
		return Paint(SGRDim, "no weekly limit", color)
	}
	left := math.Max(0, math.Min(100, math.Round(100-r.Weekly.Utilization)))
	filled := int(math.Round(left / 100 * barCells))
	bar := strings.Repeat("▰", filled) + strings.Repeat("▱", barCells-filled)
	s := fmt.Sprintf("%s %.0f%% left · ", bar, left)
	if r.Weekly.ResetsAt == nil {
		return s + Paint(sgrYellow, "not started", color)
	}
	return s + "resets " + r.Weekly.ResetsAt.In(now.Location()).Format("Mon 15:04")
}

// Align pads each column but the last to its widest cell, two spaces apart,
// and drops the trailing spaces of a row whose last cells are empty.
func Align(rows [][]string) []string {
	var widths []int
	for _, cells := range rows {
		for i, c := range cells {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], ansi.StringWidth(c))
		}
	}
	lines := make([]string, len(rows))
	for n, cells := range rows {
		var b strings.Builder
		for i, c := range cells {
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-ansi.StringWidth(c)+2))
			}
		}
		lines[n] = strings.TrimRight(b.String(), " ")
	}
	return lines
}
