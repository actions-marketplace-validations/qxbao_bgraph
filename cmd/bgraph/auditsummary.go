package main

import (
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/model"
)

// auditSummary aggregates a shadow log (`bgraph audit --summary --log FILE`).
type auditSummary struct {
	Runs        int `json:"runs"`
	Skipped     int `json:"skipped_lines"` // lines that are not audit runs
	Clean       int `json:"clean_runs"`
	CleanStreak int `json:"clean_streak"` // clean runs in a row at the end of the log
	// MissedRuns lists every run with at least one missed failure, oldest first.
	MissedRuns []missedRun `json:"missed_runs"`
	Unmapped   int         `json:"unmapped"`      // unmapped failures over all runs
	UnmappedIn int         `json:"unmapped_runs"` // runs with at least one unmapped failure
	FullRuns   int         `json:"full_runs"`     // runs whose selection was the whole suite
	FullShare  float64     `json:"full_share"`    // FullRuns / Runs
	// Selected share: selected units / total units per run (1 for a full run), over the runs whose total is known.
	Measured       int     `json:"measured_runs"`
	MeanSelected   float64 `json:"mean_selected"` // units, over Measured runs (a full run counts its total)
	MeanTotal      float64 `json:"mean_total"`
	MeanShare      float64 `json:"mean_share"`
	MedianShare    float64 `json:"median_share"`
	UnmeasuredRuns int     `json:"unmeasured_runs"` // runs without a total (logs written by older versions)
}

type missedRun struct {
	Time   string   `json:"time"`
	Base   string   `json:"base,omitempty"`
	Head   string   `json:"head,omitempty"`
	Missed int      `json:"missed"`
	Tests  []string `json:"tests"` // "<file or unit> :: <test>"
}

func summarizeAuditLog(runs []auditLogLine, skipped int) auditSummary {
	s := auditSummary{Runs: len(runs), Skipped: skipped, CleanStreak: cleanStreak(runs), MissedRuns: []missedRun{}}
	var shares []float64
	var sel, tot float64
	for _, r := range runs {
		if r.Clean {
			s.Clean++
		}
		if len(r.Missed) > 0 {
			m := missedRun{Time: r.Time, Base: r.Base, Head: r.Head, Missed: len(r.Missed)}
			for _, f := range r.Missed {
				m.Tests = append(m.Tests, failedName(f)+" :: "+f.Test)
			}
			s.MissedRuns = append(s.MissedRuns, m)
		}
		s.Unmapped += len(r.Unmapped)
		if len(r.Unmapped) > 0 {
			s.UnmappedIn++
		}
		full := r.Full || r.Scope == model.TierFull
		if full {
			s.FullRuns++
		}
		if r.Total <= 0 {
			s.UnmeasuredRuns++
			continue
		}
		n := float64(r.Selected)
		if full {
			n = float64(r.Total)
		}
		sel += n
		tot += float64(r.Total)
		shares = append(shares, n/float64(r.Total))
	}
	if s.Runs > 0 {
		s.FullShare = float64(s.FullRuns) / float64(s.Runs)
	}
	if s.Measured = len(shares); s.Measured > 0 {
		s.MeanSelected, s.MeanTotal = sel/float64(s.Measured), tot/float64(s.Measured)
		sum := 0.0
		for _, x := range shares {
			sum += x
		}
		s.MeanShare = sum / float64(s.Measured)
		slices.Sort(shares)
		if m := len(shares); m%2 == 1 {
			s.MedianShare = shares[m/2]
		} else {
			s.MedianShare = (shares[m/2-1] + shares[m/2]) / 2
		}
	}
	return s
}

func failedName(f bgraph.FailedTest) string {
	switch {
	case f.File != "":
		return f.File
	case f.Unit != "":
		return f.Unit
	}
	return "(unknown file)"
}

func runAuditSummary(path string, asJSON bool, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer f.Close()
	runs, skipped, err := readAuditLog(f)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	s := summarizeAuditLog(runs, skipped)
	if asJSON {
		return printJSON(stdout, stderr, s)
	}
	fmt.Fprintf(stdout, "runs: %d\nclean runs: %d\nclean runs in a row: %d\n", s.Runs, s.Clean, s.CleanStreak)
	if s.Skipped > 0 {
		fmt.Fprintf(stdout, "skipped lines (not audit runs): %d\n", s.Skipped)
	}
	fmt.Fprintf(stdout, "runs with missed failures: %d\n", len(s.MissedRuns))
	for _, m := range s.MissedRuns {
		fmt.Fprintf(stdout, "  %s  head %s  %d missed\n", m.Time, orDash(m.Head), m.Missed)
		for i, t := range m.Tests {
			if i == 5 {
				fmt.Fprintf(stdout, "      ... and %d more (use --json)\n", len(m.Tests)-i)
				break
			}
			fmt.Fprintf(stdout, "      %s\n", t)
		}
	}
	fmt.Fprintf(stdout, "unmapped failures: %d (in %d runs)\n", s.Unmapped, s.UnmappedIn)
	fmt.Fprintf(stdout, "full runs: %d (%.0f%%)\n", s.FullRuns, 100*s.FullShare)
	if s.Measured > 0 {
		fmt.Fprintf(stdout, "selected units: mean %.1f of %.1f; share of the suite: mean %.1f%%, median %.1f%% (%d runs)\n",
			s.MeanSelected, s.MeanTotal, 100*s.MeanShare, 100*s.MedianShare, s.Measured)
	}
	if s.UnmeasuredRuns > 0 {
		fmt.Fprintf(stdout, "runs without a unit total (older log lines): %d\n", s.UnmeasuredRuns)
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
