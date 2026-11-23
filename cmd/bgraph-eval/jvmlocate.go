package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// locName is one located FAIL_TO_PASS name, ecosystem-neutral (JVM and .NET share the report).
type locName struct {
	Name    string   `json:"name"`
	Form    string   `json:"form"`
	Class   string   `json:"class,omitempty"`
	Files   []string `json:"files,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
	Cat     string   `json:"category"` // unique | ambiguous | notfound | task
	Scanned bool     `json:"scanned,omitempty"`
	InPatch bool     `json:"in_patch,omitempty"`
	// Go only: candidate units (go:<dir>), those touched by the test patch, and whether the import path in the name picked them.
	Units      []string `json:"units,omitempty"`
	PatchUnits []string `json:"patch_units,omitempty"`
	PkgHit     bool     `json:"pkg_hit,omitempty"`
	// Ruby only: how the candidates were found, helper files (shared examples) replaced by their includers, and names located
	// outside a Ruby test unit (Cucumber scenarios; counted as notfound).
	Via     string   `json:"via,omitempty"`
	Helpers []string `json:"helpers,omitempty"`
	NotUnit bool     `json:"not_unit,omitempty"`
}

type locRow struct {
	ID     string    `json:"instance_id"`
	Repo   string    `json:"repo"`
	Source string    `json:"source"`
	Kind   string    `json:"f2p_kind,omitempty"`
	F2P    int       `json:"f2p_count"`
	Err    string    `json:"error,omitempty"`
	Names  []locName `json:"names"`
}

func jvmLocName(l evalkit.JVMTestLoc) locName {
	n := locName{Name: l.Name, Form: l.Form, Class: l.Class, Files: l.Files, Dirs: l.Dirs, Scanned: l.Scanned, InPatch: l.InPatch}
	switch {
	case l.Task:
		n.Cat = "task"
	case l.NotFound:
		n.Cat = "notfound"
	case l.Ambiguous:
		n.Cat = "ambiguous"
	default:
		n.Cat = "unique"
	}
	return n
}

func goLocName(l evalkit.GoTestLoc) locName {
	n := locName{Name: l.Name, Form: l.Form, Class: l.Func, Files: l.Files, InPatch: l.InPatch, Units: l.Units, PatchUnits: l.PatchUnits, PkgHit: l.PkgHit}
	switch {
	case l.NotFound:
		n.Cat = "notfound"
	case l.Ambiguous:
		n.Cat = "ambiguous"
	default:
		n.Cat = "unique"
	}
	return n
}

// rbLocName: Ruby units are test files, so Units = Files (unit ID = repo-relative path).
func rbLocName(l evalkit.RbTestLoc) locName {
	n := locName{Name: l.Name, Form: l.Form, Class: l.Class, Files: l.Files, InPatch: l.InPatch, Units: l.Files, PatchUnits: l.PatchFiles,
		Via: l.Via, Helpers: l.Helpers, NotUnit: l.NotUnit}
	switch {
	case l.NotFound || l.NotUnit:
		n.Cat = "notfound"
	case l.Ambiguous:
		n.Cat = "ambiguous"
	default:
		n.Cat = "unique"
	}
	return n
}

func dotnetLocName(l evalkit.DotnetTestLoc) locName {
	n := locName{Name: l.Name, Form: l.Form, Class: l.Class, Files: l.Files, Scanned: l.Scanned, InPatch: l.InPatch}
	switch {
	case l.NotFound:
		n.Cat = "notfound"
	case l.Ambiguous:
		n.Cat = "ambiguous"
	default:
		n.Cat = "unique"
	}
	return n
}

// cmdJVMLocate runs the JVM (eco "jvm") or .NET (eco "dotnet") test-name locator over every task's P tree and prints unique / ambiguous /
// not-found statistics (overall, per repo, per name form, and for n2p tasks how many names fall into files the test patch touches).
func cmdJVMLocate(ctx context.Context, args []string, eco string) error {
	fs := flag.NewFlagSet(eco+"-locate", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL (tasks-"+eco+".jsonl)")
	work := fs.String("work", "", "work directory (clones in <work>/repos)")
	out := fs.String("out", "", "write per-name locations as JSON here")
	examples := fs.Int("examples", 3, "examples printed per category and form")
	jobs := fs.Int("jobs", 4, "repos processed in parallel")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" {
		return fmt.Errorf("--tasks and --work are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	byRepo := map[string][]evalkit.Task{}
	for _, t := range tasks {
		k := strings.ToLower(t.Repo)
		byRepo[k] = append(byRepo[k], t)
	}
	var mu sync.Mutex
	var rows []locRow
	var wg sync.WaitGroup
	sem := make(chan struct{}, *jobs)
	for _, ts := range byRepo {
		wg.Add(1)
		go func(ts []evalkit.Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for i, t := range ts {
				if ctx.Err() != nil {
					return
				}
				idx := filepath.Join(*work, "idx", fmt.Sprintf("%sloc_%s_%d.index", eco, strings.ReplaceAll(t.Repo, "/", "__"), i))
				os.MkdirAll(filepath.Dir(idx), 0o755)
				dir := evalkit.RustRepoDir(*work, t.Repo)
				var r locRow
				if eco == "ruby" {
					x := evalkit.RbLocateTask(ctx, dir, idx, t)
					r = locRow{ID: x.ID, Repo: x.Repo, Source: x.Source, Kind: x.Kind, F2P: x.F2P, Err: x.Err}
					for _, l := range x.Locs {
						r.Names = append(r.Names, rbLocName(l))
					}
				} else if eco == "go" {
					x := evalkit.GoLocateTask(ctx, dir, idx, t)
					r = locRow{ID: x.ID, Repo: x.Repo, Source: x.Source, Kind: x.Kind, F2P: x.F2P, Err: x.Err}
					for _, l := range x.Locs {
						r.Names = append(r.Names, goLocName(l))
					}
				} else if eco == "dotnet" {
					x := evalkit.DotnetLocateTask(ctx, dir, idx, t)
					r = locRow{ID: x.ID, Repo: x.Repo, Source: x.Source, F2P: x.F2P, Err: x.Err}
					for _, l := range x.Locs {
						r.Names = append(r.Names, dotnetLocName(l))
					}
				} else {
					x := evalkit.JVMLocateTask(ctx, dir, idx, t)
					r = locRow{ID: x.ID, Repo: x.Repo, Source: x.Source, Kind: x.Kind, F2P: x.F2P, Err: x.Err}
					for _, l := range x.Locs {
						r.Names = append(r.Names, jvmLocName(l))
					}
				}
				mu.Lock()
				rows = append(rows, r)
				c := map[string]int{}
				for _, n := range r.Names {
					c[n.Cat]++
				}
				fmt.Fprintf(os.Stderr, "%-48s f2p=%-5d unique=%-5d ambiguous=%-4d notfound=%-4d task=%-2d %s\n", r.ID, r.F2P, c["unique"], c["ambiguous"], c["notfound"], c["task"], r.Err)
				mu.Unlock()
			}
		}(ts)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	printLocateReport(rows, *examples)
	if *out != "" {
		b, _ := json.MarshalIndent(rows, "", " ")
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

type locAgg struct{ tasks, names, uniq, amb, nf, task, errs, scanned int }

func (a *locAgg) add(n locName) {
	a.names++
	switch n.Cat {
	case "unique":
		a.uniq++
	case "ambiguous":
		a.amb++
	case "notfound":
		a.nf++
	case "task":
		a.task++
	}
	if n.Scanned {
		a.scanned++
	}
}

func printLocateReport(rows []locRow, examples int) {
	by, form := map[string]*locAgg{}, map[string]*locAgg{}
	get := func(m map[string]*locAgg, k string) *locAgg {
		if m[k] == nil {
			m[k] = &locAgg{}
		}
		return m[k]
	}
	// n2p tasks list the whole suite; the oracle keeps only the names in files the test patch touches.
	var n2pNames, n2pIn, n2pInNF, n2pTasks int
	for _, r := range rows {
		for _, k := range []string{"ALL", "repo=" + strings.ToLower(r.Repo)} {
			a := get(by, k)
			a.tasks++
			if r.Err != "" {
				a.errs++
			}
			for _, n := range r.Names {
				a.add(n)
			}
		}
		if r.Kind == "n2p" {
			n2pTasks++
		}
		for _, n := range r.Names {
			get(form, n.Form).add(n)
			if r.Kind == "n2p" {
				n2pNames++
				if n.InPatch {
					n2pIn++
				}
			}
		}
		if r.Kind == "n2p" {
			in := 0
			for _, n := range r.Names {
				if n.InPatch {
					in++
				}
			}
			if in == 0 {
				n2pInNF++
			}
		}
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%-44s %6s %7s %7s %7s %9s %5s %7s %5s\n", "group", "tasks", "names", "unique", "ambig", "notfound", "task", "scanned", "errs")
	for _, k := range keys {
		a := by[k]
		fmt.Printf("%-44s %6d %7d %7d %7d %9d %5d %7d %5d\n", k, a.tasks, a.names, a.uniq, a.amb, a.nf, a.task, a.scanned, a.errs)
	}
	fmt.Println()
	fk := make([]string, 0, len(form))
	for k := range form {
		fk = append(fk, k)
	}
	sort.Strings(fk)
	for _, k := range fk {
		a := form[k]
		fmt.Printf("%-44s %6s %7d %7d %7d %9d %5d %7d\n", "form="+k, "", a.names, a.uniq, a.amb, a.nf, a.task, a.scanned)
	}
	fmt.Printf("\nn2p tasks: %d, names %d, names in files touched by the test patch %d; tasks with no such name %d\n", n2pTasks, n2pNames, n2pIn, n2pInNF)
	cat := map[string][]string{}
	seen := map[string]int{}
	for _, r := range rows {
		for _, n := range r.Names {
			k := n.Cat + "|" + n.Form
			if n.Scanned {
				k += "|scanned"
			}
			if seen[k] < examples {
				seen[k]++
				f := n.Files
				if len(n.Dirs) > 0 {
					f = n.Dirs
				}
				if len(f) > 3 {
					f = append(append([]string{}, f[:3]...), fmt.Sprintf("... +%d", len(n.Files)-3))
				}
				cat[k] = append(cat[k], fmt.Sprintf("%s  %.110s -> %v", r.ID, n.Name, f))
			}
		}
	}
	ck := make([]string, 0, len(cat))
	for k := range cat {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	for _, k := range ck {
		fmt.Printf("\nexamples (%s):\n", strings.ReplaceAll(k, "|", ", "))
		for _, e := range cat[k] {
			fmt.Println("  " + e)
		}
	}
}
