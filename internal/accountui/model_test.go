package accountui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"remote-agents/internal/accounts"
)

var (
	now    = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	resets = time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
)

func listing() accounts.Listing {
	return accounts.Listing{Accounts: []accounts.Row{
		{Name: "invoices", Email: "i@example.com", Weekly: &accounts.Window{Utilization: 65, ResetsAt: &resets}},
		{Name: "juljus", Email: "j@example.com", Active: true, Weekly: &accounts.Window{Utilization: 51, ResetsAt: &resets}},
		{Name: "molton", Email: "m@example.com", Weekly: &accounts.Window{}},
	}}
}

func runes(s string) []Key {
	var keys []Key
	for _, r := range s {
		keys = append(keys, Key{Code: KeyRune, Rune: r})
	}
	return keys
}

// press hands the model each key, and returns the last action.
func press(m *Model, keys ...Key) Action {
	var a Action
	for _, k := range keys {
		a = m.Key(k)
	}
	return a
}

var (
	up    = Key{Code: KeyUp}
	down  = Key{Code: KeyDown}
	enter = Key{Code: KeyEnter}
	esc   = Key{Code: KeyEsc}
	bs    = Key{Code: KeyBackspace}
)

func TestPickerStartsOnTheActiveAccountAndShowsTheTable(t *testing.T) {
	m := New(listing())
	got := strings.Join(m.View(200, now, false), "\n")
	want := strings.Join([]string{
		"  invoices  i@example.com  ▰▰▱▱▱ 35% left · resets Thu 18:00",
		"▸ juljus    j@example.com  ▰▰▱▱▱ 49% left · resets Thu 18:00  active",
		"  molton    m@example.com  ▰▰▰▰▰ 100% left · not started",
		"",
		"",
		"↑↓ move · enter switch · a add · e rename · d remove · q quit",
	}, "\n")
	if got != want {
		t.Fatalf("view:\n%s\nwant:\n%s", got, want)
	}
}

func TestPickerMovesWithArrowsVimKeysAndDigits(t *testing.T) {
	m := New(listing())
	steps := []struct {
		key  Key
		want string
	}{
		{up, "invoices"}, {up, "invoices"}, {down, "juljus"}, {Key{Code: KeyRune, Rune: 'j'}, "molton"},
		{down, "molton"}, {Key{Code: KeyRune, Rune: 'k'}, "juljus"}, {Key{Code: KeyRune, Rune: '1'}, "invoices"},
		{Key{Code: KeyRune, Rune: '9'}, "invoices"}, {Key{Code: KeyRune, Rune: '3'}, "molton"},
	}
	for i, s := range steps {
		m.Key(s.key)
		if row, _ := m.current(); row.Name != s.want {
			t.Fatalf("step %d: on %q, want %q", i, row.Name, s.want)
		}
	}
}

func TestPickerSwitchesToTheHighlightedAccount(t *testing.T) {
	m := New(listing())
	if a := press(&m, up, enter); a != (Action{Kind: ActSwitch, Name: "invoices"}) {
		t.Fatalf("enter = %+v", a)
	}
}

func TestPickerLeavesOnEscQAndCtrlC(t *testing.T) {
	for _, k := range []Key{esc, {Code: KeyRune, Rune: 'q'}, {Code: KeyCtrlC}} {
		m := New(listing())
		if a := m.Key(k); a.Kind != ActQuit {
			t.Fatalf("%+v = %+v, want quit", k, a)
		}
	}
}

func TestPickerSaysTheActiveAccountIsActiveAlready(t *testing.T) {
	m := New(listing())
	if a := m.Key(enter); a.Kind != ActNone {
		t.Fatalf("enter on the active account = %+v", a)
	}
	if got := m.View(200, now, false)[4]; got != "juljus is the box's account already" {
		t.Fatalf("status = %q", got)
	}
}

func TestPickerAddsAnAccountByName(t *testing.T) {
	m := New(listing())
	m.Key(Key{Code: KeyRune, Rune: 'a'})
	press(&m, runes("worx")...)
	m.Key(bs)
	press(&m, runes("k")...)
	if got := m.View(200, now, false)[4]; got != "name of the new account: work▏" {
		t.Fatalf("prompt = %q", got)
	}
	if got := m.View(200, now, false)[5]; got != "enter save · esc cancel" {
		t.Fatalf("footer = %q", got)
	}
	if a := m.Key(enter); a != (Action{Kind: ActAdd, Name: "work"}) {
		t.Fatalf("enter = %+v", a)
	}
}

func TestPickerKeepsANameItCannotSaveOnThePrompt(t *testing.T) {
	m := New(listing())
	press(&m, append([]Key{{Code: KeyRune, Rune: 'a'}}, runes("Bad")...)...)
	if a := m.Key(enter); a.Kind != ActNone {
		t.Fatalf("enter = %+v", a)
	}
	view := m.View(200, now, false)
	if !strings.HasPrefix(view[4], "name of the new account: Bad") || !strings.Contains(view[5], "esc cancel") {
		t.Fatalf("view after a bad name:\n%s", strings.Join(view, "\n"))
	}
	m.Key(esc)
	if got := m.View(200, now, false)[5]; !strings.HasPrefix(got, "↑↓ move") {
		t.Fatalf("esc did not cancel: footer %q", got)
	}
}

func TestPickerRenamesTheHighlightedAccount(t *testing.T) {
	m := New(listing())
	m.Key(Key{Code: KeyRune, Rune: 'e'})
	if got := m.View(200, now, false)[4]; got != "rename juljus to: juljus▏" {
		t.Fatalf("prompt = %q", got)
	}
	press(&m, bs, bs, bs, bs, bs, bs)
	press(&m, runes("me")...)
	if a := m.Key(enter); a != (Action{Kind: ActRename, Name: "juljus", NewName: "me"}) {
		t.Fatalf("enter = %+v", a)
	}
}

func TestPickerRenameToTheSameNameDoesNothing(t *testing.T) {
	m := New(listing())
	if a := press(&m, Key{Code: KeyRune, Rune: 'e'}, enter); a.Kind != ActNone {
		t.Fatalf("enter = %+v", a)
	}
}

func TestPickerNamesTheUnsavedLogin(t *testing.T) {
	l := listing()
	l.Accounts[1].Active = false
	l.Accounts = append(l.Accounts, accounts.Row{Email: "stray@example.com", Active: true})
	m := New(l)
	if got := m.View(200, now, false)[3]; !strings.HasPrefix(got, "▸ unsaved   stray@example.com") {
		t.Fatalf("unsaved row = %q", got)
	}
	m.Key(enter)
	if got := m.View(200, now, false)[5]; got != "the box's login has no name yet; press e to name it" {
		t.Fatalf("status = %q", got)
	}
	m.Key(Key{Code: KeyRune, Rune: 'e'})
	if got := m.View(200, now, false)[5]; got != "name the box's login: ▏" {
		t.Fatalf("prompt = %q", got)
	}
	press(&m, runes("stray")...)
	if a := m.Key(enter); a != (Action{Kind: ActSave, Name: "stray"}) {
		t.Fatalf("enter = %+v", a)
	}
}

func TestPickerRemovesOnlyAfterYes(t *testing.T) {
	m := New(listing())
	m.Key(Key{Code: KeyRune, Rune: 'd'})
	if got := m.View(200, now, false)[4]; got != "remove juljus? y/N" {
		t.Fatalf("confirm = %q", got)
	}
	if a := m.Key(Key{Code: KeyRune, Rune: 'n'}); a.Kind != ActNone {
		t.Fatalf("n = %+v", a)
	}
	if got := m.View(200, now, false)[4]; got != "kept juljus" {
		t.Fatalf("status = %q", got)
	}
	if a := press(&m, Key{Code: KeyRune, Rune: 'd'}, Key{Code: KeyRune, Rune: 'y'}); a != (Action{Kind: ActRemove, Name: "juljus"}) {
		t.Fatalf("d y = %+v", a)
	}
}

func TestPickerOffersToStartOnlyAWindowThatHasNotStarted(t *testing.T) {
	m := New(listing())
	if a := m.Key(Key{Code: KeyRune, Rune: 's'}); a.Kind != ActNone {
		t.Fatalf("s on a started window = %+v", a)
	}
	m.Key(down)
	if got := m.View(200, now, false)[5]; got != "↑↓ move · enter switch · a add · e rename · d remove · s start week · q quit" {
		t.Fatalf("footer = %q", got)
	}
	if a := m.Key(Key{Code: KeyRune, Rune: 's'}); a != (Action{Kind: ActStart, Name: "molton"}) {
		t.Fatalf("s = %+v", a)
	}
}

func TestPickerKeepsItsCursorOnTheAccountAcrossAListing(t *testing.T) {
	m := New(listing())
	m.Key(up)
	l := listing()
	l.Accounts = l.Accounts[1:]
	m.SetListing(l, "")
	if row, _ := m.current(); row.Name != "juljus" {
		t.Fatalf("lost account: on %q, want the first row once invoices is gone", row.Name)
	}
	m.SetListing(listing(), "molton")
	if row, _ := m.current(); row.Name != "molton" {
		t.Fatalf("on %q, want molton", row.Name)
	}
}

func TestPickerCutsLinesToTheTerminal(t *testing.T) {
	m := New(listing())
	for _, line := range m.View(20, now, true) {
		if w := ansi.StringWidth(line); w > 20 {
			t.Fatalf("%q is %d wide", line, w)
		}
	}
}

func TestPickerOffersNoStartForTheUnsavedLogin(t *testing.T) {
	m := New(accounts.Listing{Accounts: []accounts.Row{{Email: "stray@example.com", Active: true, Weekly: &accounts.Window{}}}})
	if a := m.Key(Key{Code: KeyRune, Rune: 's'}); a.Kind != ActNone {
		t.Fatalf("s = %+v", a)
	}
	if got := m.View(200, now, false)[3]; strings.Contains(got, "start week") {
		t.Fatalf("footer %q", got)
	}
}
