package jvmdyn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ClassLoad is one class the agent saw defined.
type ClassLoad struct {
	Name       string // internal name a/b/C$D
	Location   string // code source URL ("" when none)
	SourceFile string // SourceFile attribute ("" when none)
	SMAP       []SMAPFile
}

// SMAPFile is one entry of the *F section of a SourceDebugExtension (Kotlin inline functions: the file whose body was inlined).
type SMAPFile struct{ Name, Path string }

// Fork is what one test JVM logged (one agent log plus its JFR dump).
type Fork struct {
	Log       string // the log file
	JavaHome  string
	Command   string // sun.java.command
	Complete  bool   // the log has its end line
	JFR       string // "" ok, else why the file events are missing
	Hook      string // "" ok, else why the mmap hook is missing
	Classes   []ClassLoad
	Unseen    []string // "name\tlocation" of loaded classes the transformer never saw
	MMaps     []string // mapped paths ("" when unknown)
	Reads     []string // file reads (absolute paths as the JVM saw them)
	ClassFile []ClassLoad
	Processes []string
	Natives   []string
}

func field(fs []string, i int) string {
	if i < len(fs) && fs[i] != "-" {
		return fs[i]
	}
	return ""
}

func parseSMAP(s string) []SMAPFile {
	if s == "" || s == "-" {
		return nil
	}
	var out []SMAPFile
	for _, e := range strings.Split(s, ";") {
		if e == "" {
			continue
		}
		n, p, _ := strings.Cut(e, "|")
		out = append(out, SMAPFile{Name: n, Path: p})
	}
	return out
}

// ParseLog reads one agent log (the line format is fixed by Agent.java).
func ParseLog(file string) (Fork, error) {
	f := Fork{Log: file, JFR: "no jfr status line", Hook: "no hook status line"}
	b, err := os.ReadFile(file)
	if err != nil {
		return f, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<26)
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		fs := strings.Split(line, "\t")
		if first {
			first = false
			if fs[0] != "start" {
				return f, fmt.Errorf("%s: no start line", file)
			}
			f.JavaHome, f.Command = field(fs, 2), field(fs, 4)
			continue
		}
		switch fs[0] {
		case "jfr":
			f.JFR = ""
			if field(fs, 1) != "ok" {
				f.JFR = "JFR not started: " + field(fs, 2)
			}
		case "hook":
			f.Hook = ""
			if field(fs, 1) != "ok" {
				f.Hook = "mmap hook not installed: " + field(fs, 2)
			}
		case "class":
			f.Classes = append(f.Classes, ClassLoad{Name: field(fs, 1), Location: field(fs, 2), SourceFile: field(fs, 3), SMAP: parseSMAP(field(fs, 4))})
		case "unseen":
			f.Unseen = append(f.Unseen, field(fs, 1)+"\t"+field(fs, 2))
		case "mmap":
			f.MMaps = append(f.MMaps, field(fs, 1))
		case "end":
			f.Complete = true
		}
	}
	return f, sc.Err()
}

// dumpEvents runs the agent's JFR dumper over jfr files (batched to stay below command-line limits) and adds the events to
// the forks, keyed by the stem shared by the .log and .jfr files. A fork without a dump keeps a JFR reason.
func dumpEvents(ctx context.Context, javaHome, agentJar string, forks map[string]*Fork, env []string) error {
	var files []string
	for stem := range forks {
		jfr := stem + ".jfr"
		if _, err := os.Stat(jfr); err == nil {
			files = append(files, jfr)
		} else if forks[stem].JFR == "" {
			forks[stem].JFR = "no JFR file"
		}
	}
	sort.Strings(files)
	seen := map[string]bool{}
	for len(files) > 0 {
		n, size := 0, 0
		for n < len(files) && size+len(files[n]) < 20000 {
			size += len(files[n]) + 1
			n++
		}
		batch := files[:n]
		files = files[n:]
		cmd := exec.CommandContext(ctx, jdkTool(javaHome, "java"), append([]string{"-cp", agentJar, "bgraph.agent.Dump"}, batch...)...)
		cmd.Env = cleanToolEnv(env)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("JFR dump: %v: %s", err, tail(stderr.String(), 400))
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimRight(line, "\r")
			fs := strings.Split(line, "\t")
			if len(fs) < 2 {
				continue
			}
			stem := strings.TrimSuffix(fs[0], ".jfr")
			f := forks[stem]
			if f == nil {
				continue
			}
			seen[stem] = true
			switch fs[1] {
			case "read":
				if p := field(fs, 2); p != "" {
					f.Reads = append(f.Reads, p)
				}
			case "process":
				f.Processes = append(f.Processes, field(fs, 2))
			case "native":
				f.Natives = append(f.Natives, field(fs, 2))
			case "classfile":
				f.ClassFile = append(f.ClassFile, ClassLoad{Name: field(fs, 3), Location: field(fs, 2), SourceFile: field(fs, 4), SMAP: parseSMAP(field(fs, 5))})
			case "error":
				f.JFR = "unreadable JFR file: " + field(fs, 2)
			}
		}
		for _, b := range batch {
			stem := strings.TrimSuffix(b, ".jfr")
			if !seen[stem] && forks[stem].JFR == "" {
				// A recording without a single event still has native libraries (period endChunk): no output means no dump.
				forks[stem].JFR = "empty JFR dump"
			}
		}
	}
	return nil
}

// cleanToolEnv is the environment of bgraph's own JVM runs: without JAVA_TOOL_OPTIONS (the agent must not record the dumper).
func cleanToolEnv(env []string) []string {
	var out []string
	for _, kv := range append(os.Environ(), env...) {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "JAVA_TOOL_OPTIONS") || strings.EqualFold(k, "JDK_JAVA_OPTIONS") || strings.EqualFold(k, "_JAVA_OPTIONS") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// LoadForks reads every agent log of dir with its JFR events.
func LoadForks(ctx context.Context, dir, javaHome, agentJar string, env []string) ([]*Fork, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	forks := map[string]*Fork{}
	var order []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := ParseLog(p)
		if err != nil {
			f = Fork{Log: p, JFR: "unreadable log", Hook: err.Error()}
		}
		stem := strings.TrimSuffix(p, ".log")
		forks[stem] = &f
		order = append(order, stem)
	}
	if len(forks) > 0 {
		if err := dumpEvents(ctx, javaHome, agentJar, forks, env); err != nil {
			return nil, err
		}
	}
	sort.Strings(order)
	out := make([]*Fork, 0, len(order))
	for _, s := range order {
		out = append(out, forks[s])
	}
	return out, nil
}

// locationPath turns a code-source URL into a local path ("" when it is not a file: URL).
func locationPath(loc string) string {
	if !strings.HasPrefix(loc, "file:") {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	p := u.Path
	if p == "" {
		p = u.Opaque
	}
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}
