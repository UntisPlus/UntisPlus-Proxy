package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"untis-proxy/internal/proxy"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// cmdRecon runs a manual teacher/room/subject sweep against the school server.
// It is the operator escape hatch for a registry that is empty, stale or was
// rebuilt with the wrong classes: the background scan resumes and skips, while
// this command can be told to start over and can be paced either hard (many
// fetches at once) or slow (one at a time with a delay).
func cmdRecon(st *store.Store, args []string, server, school string) {
	fs := flag.NewFlagSet("recon", flag.ExitOnError)
	mode := fs.String("mode", "slow", "hard fetches several classes at once; slow fetches one at a time with a delay")
	full := fs.Bool("full", false, "ignore saved scan horizons and re-read each class from the year start")
	concurrency := fs.Int("concurrency", 0, "parallel fetches (default: hard=8, slow=1)")
	delay := fs.Duration("delay", -1, "pause after each chunk (default: hard=0s, slow=1s)")
	yearStart := fs.String("year-start", "", "first date to scan (default: start of the current German school year)")
	yearEnd := fs.String("year-end", "", "fallback last date when the horizon is unavailable (default: end of the school year)")
	dryRun := fs.Bool("dry-run", false, "show how many classes and chunks the sweep would read, then stop")
	fs.Parse(args)

	if school == "" {
		fatal("recon: -school or $UNTIS_SCHOOL is required")
	}
	m := proxy.ReconMode(strings.ToLower(*mode))
	if m != proxy.ReconHard && m != proxy.ReconSlow {
		fatal("recon: -mode must be \"hard\" or \"slow\", got %q", *mode)
	}
	ys, ye := proxy.SchoolYearRange(time.Now())
	if *yearStart != "" {
		ys = *yearStart
	}
	if *yearEnd != "" {
		ye = *yearEnd
	}

	uc := untis.New(untis.Config{Server: server, School: school})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := proxy.ReconOptions{
		School:      school,
		YearStart:   ys,
		YearEnd:     ye,
		Mode:        m,
		Full:        *full,
		Concurrency: *concurrency,
		Delay:       *delay,
		DryRun:      *dryRun,
	}
	opts.Progress = newReconPrinter(os.Stdout, m)

	fmt.Printf("recon %s: school=%s year=%s..%s full=%v concurrency=%s delay=%s\n",
		m, school, ys, ye, *full, concurrencyLabel(*concurrency, m), delayLabel(*delay, m))
	if *dryRun {
		fmt.Println("dry run: no upstream requests, no database writes")
	}

	res, err := proxy.RunRecon(ctx, st, uc, opts)
	if err != nil {
		fmt.Println()
		fatal("recon: %v", err)
	}
	fmt.Println()
	fmt.Printf("recon done in %s: %d classes (%d already at horizon, %d failed), %d chunks",
		res.Elapsed.Round(time.Millisecond), res.Classes, res.ClassesFresh, res.ClassesFailed, res.Chunks)
	if *dryRun {
		fmt.Println()
		return
	}
	teachers, rooms, subjects := res.Elements["TEACHER"], res.Elements["ROOM"], res.Elements["SUBJECT"]
	fmt.Printf(", elements: %d teachers, %d rooms, %d subjects\n", teachers, rooms, subjects)
}

func concurrencyLabel(c int, m proxy.ReconMode) string {
	if c > 0 {
		return fmt.Sprint(c)
	}
	if m == proxy.ReconHard {
		return "8 (hard)"
	}
	return "1 (slow)"
}

func delayLabel(d time.Duration, m proxy.ReconMode) string {
	if d >= 0 {
		return d.String()
	}
	if m == proxy.ReconHard {
		return "0s (hard)"
	}
	return "1s (slow)"
}

// reconPrinter draws the live percentage. Callbacks arrive from several worker
// goroutines in hard mode, so the writes are serialized. On a terminal the bar
// is redrawn in place; when output is redirected (logs, CI) it prints a new
// line at each 5% step instead of a wall of carriage returns.
type reconPrinter struct {
	mu       sync.Mutex
	out      *os.File
	terminal bool
	lastPct  int
	lastLen  int
}

func newReconPrinter(out *os.File, _ proxy.ReconMode) func(proxy.ReconProgress) {
	fi, err := out.Stat()
	p := &reconPrinter{out: out, terminal: err == nil && fi.Mode()&os.ModeCharDevice != 0, lastPct: -1}
	return p.print
}

func (p *reconPrinter) print(pr proxy.ReconProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pct := 0
	if pr.ChunksTotal > 0 {
		pct = pr.ChunksDone * 100 / pr.ChunksTotal
	}
	errNote := ""
	if pr.Err != nil {
		errNote = fmt.Sprintf(" · class %d: %v", pr.ClassID, pr.Err)
	}
	line := fmt.Sprintf("%3d%% [%s] %d/%d chunks · %d/%d classes%s",
		pct, progressBar(pct, 24), pr.ChunksDone, pr.ChunksTotal,
		pr.ClassesDone, pr.ClassesTotal, errNote)

	if !p.terminal {
		// Only report steps, not every class, so redirected output stays useful.
		if pct < 100 && pct/5 == p.lastPct/5 && pr.Err == nil {
			p.lastPct = pct
			return
		}
		p.lastPct = pct
		fmt.Fprintln(p.out, line)
		return
	}
	// Pad to overwrite a longer previous line (an error message can be long).
	pad := ""
	if n := len(line); n < p.lastLen {
		pad = strings.Repeat(" ", p.lastLen-n)
	}
	p.lastLen = len(line)
	fmt.Fprintf(p.out, "\r%s%s", line, pad)
	p.lastPct = pct
}

func progressBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	return strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
}
