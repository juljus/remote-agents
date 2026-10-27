package reaper

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Proc struct {
	PID, PPID int
	// SID is the process's session; a daemon that detached itself leads its own.
	SID    int
	Zombie bool
	Args   []string
	// CPU is the user and system time used so far, in clock ticks, including
	// that of waited children: a build's compilers live too briefly to be seen.
	CPU uint64
}

// ReadProcs lists the processes of a Linux /proc tree.
func ReadProcs(proc fs.FS) ([]Proc, error) {
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readProc(proc, pid)
		// A process can exit between the listing and the read.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func readProc(proc fs.FS, pid int) (Proc, error) {
	stat, err := fs.ReadFile(proc, strconv.Itoa(pid)+"/stat")
	if err != nil {
		return Proc{}, err
	}
	// The command name in parentheses may itself contain spaces and parentheses.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return Proc{}, fmt.Errorf("process %d: malformed stat %q", pid, stat)
	}
	// fields[0] is stat's field 3 (state); utime, stime, cutime and cstime are fields 14 to 17.
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 15 {
		return Proc{}, fmt.Errorf("process %d: malformed stat %q", pid, stat)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Proc{}, fmt.Errorf("process %d: parent %q: %w", pid, fields[1], err)
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return Proc{}, fmt.Errorf("process %d: session %q: %w", pid, fields[3], err)
	}
	var cpu uint64
	for _, f := range fields[11:15] {
		ticks, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return Proc{}, fmt.Errorf("process %d: cpu time %q: %w", pid, f, err)
		}
		cpu += ticks
	}
	cmdline, err := fs.ReadFile(proc, strconv.Itoa(pid)+"/cmdline")
	if err != nil {
		return Proc{}, err
	}
	var args []string
	if trimmed := bytes.TrimSuffix(cmdline, []byte{0}); len(trimmed) > 0 {
		for _, a := range bytes.Split(trimmed, []byte{0}) {
			args = append(args, string(a))
		}
	}
	return Proc{PID: pid, PPID: ppid, SID: sid, Zombie: fields[0] == "Z", Args: args, CPU: cpu}, nil
}

// Exited reports whether pid is gone or only waits to be collected.
func Exited(proc fs.FS, pid int) (bool, error) {
	p, err := readProc(proc, pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return p.Zombie, nil
}

// toolShell reports a shell Claude Code's Bash tool runs a command in.
func toolShell(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.Contains(args[i+1], "/.claude/shell-snapshots/snapshot-") {
			return true
		}
	}
	return false
}

// ToolShells counts the Bash tool shells below agent that are still running a command.
func ToolShells(procs []Proc, agent int) int {
	children := childrenOf(procs)
	n := 0
	stack := []int{agent}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[pid] {
			if toolShell(c.Args) {
				n++
				continue
			}
			stack = append(stack, c.PID)
		}
	}
	return n
}

// Leftovers splits what the agent's tools left behind, which the launcher
// adopted as subreaper. Daemons that detached into a session of their own
// (a tmux server, an ssh ControlMaster, gpg-agent) may serve other sessions,
// so they and everything below them are apart from the rest; Bash tool shells
// lead their own session too but are no daemons.
func Leftovers(procs []Proc, launcher, agent int) (own, daemons []Proc) {
	children := childrenOf(procs)
	var stack []Proc
	for _, c := range children[launcher] {
		if c.PID != agent {
			stack = append(stack, c)
		}
	}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p.SID == p.PID && !toolShell(p.Args) {
			daemons = append(daemons, p)
			continue
		}
		own = append(own, p)
		stack = append(stack, children[p.PID]...)
	}
	return own, daemons
}

func childrenOf(procs []Proc) map[int][]Proc {
	children := map[int][]Proc{}
	for _, p := range procs {
		if !p.Zombie {
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	return children
}

// clockTicks is Linux's USER_HZ, the unit of /proc's CPU times on every architecture.
const clockTicks = 100

// busyShare of one core is the least CPU that counts left-behind processes as working:
// a build uses far more, an idle daemon (gpg-agent, a Gradle daemon) far less.
const busyShare = 0.02

// BusyTicks is the CPU time over one poll that counts as busy.
func BusyTicks(poll time.Duration) uint64 {
	return max(1, uint64(poll.Seconds()*clockTicks*busyShare))
}

// CPUMeter measures the CPU time processes used between two looks.
type CPUMeter struct{ last map[int]uint64 }

// Used is the CPU time procs used since the previous call; a process seen
// for the first time adds nothing.
func (m *CPUMeter) Used(procs []Proc) uint64 {
	now := make(map[int]uint64, len(procs))
	var used uint64
	for _, p := range procs {
		now[p.PID] = p.CPU
		if prev, ok := m.last[p.PID]; ok && p.CPU > prev {
			used += p.CPU - prev
		}
	}
	m.last = now
	return used
}
