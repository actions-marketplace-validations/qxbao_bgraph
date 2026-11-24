package rbdyn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

// parseScript compiles each (name, base file, head file) triple of ARGV with warnings on and prints, per file, whether the head
// compiles and whether its parse-time warnings (without file and line) equal the base's.
const parseScript = `$VERBOSE = true
$bgraph_w = []
module Warning
  def self.warn(msg, **)
    $bgraph_w << msg.to_s.sub(/\A[^\n]*?:\d+:\s*/, "")
  end
end
ARGV.each_slice(3) do |name, b, h|
  out = [b, h].map do |f|
    $bgraph_w = []
    err = nil
    begin
      RubyVM::InstructionSequence.compile(File.binread(f), name, name)
    rescue SyntaxError, ScriptError, StandardError => e
      err = e.message.to_s.lines.first.to_s.strip
    end
    [$bgraph_w.sort, err]
  end
  if out[0][1]
    puts "ERR #{name}: does not compile at the base (#{out[0][1]})"
  elsif out[1][1]
    puts "ERR #{name}: does not compile at the head (#{out[1][1]})"
  elsif out[0][0] != out[1][0]
    puts "ERR #{name}: the change alters the warnings Ruby emits when it loads the file (#{(out[1][0] - out[0][0]).first.to_s.strip[0, 120]})"
  else
    defs = [b, h].map do |f|
      found = []
      walk = lambda do |iseq|
        a = iseq.to_a
        found << "#{a[4][:code_location][0]}-#{a[4][:code_location][2]}" if a[9] == :method
        iseq.each_child { |c| walk.call(c) }
      end
      walk.call(RubyVM::InstructionSequence.compile(File.binread(f), name, name))
      found.join(",")
    end
    puts "OK #{name}|#{defs[0]}|#{defs[1]}"
  end
end
`

// ParseFile is one changed file whose changes the body guard accepted.
type ParseFile struct {
	Path       string
	Base, Head []byte
}

// sameDefs reports whether the method spans Ruby compiled (rubyDefs: "start-end" joined by commas) are exactly the defs the
// guard's structure found in src: the guard's method bodies, and therefore its skeleton, are Ruby's own.
func sameDefs(src []byte, rubyDefs string) bool {
	f, why := parseRuby(src)
	if why != "" {
		return false
	}
	left := map[[2]int]int{}
	if rubyDefs != "" {
		for _, d := range strings.Split(rubyDefs, ",") {
			a, b, ok := strings.Cut(d, "-")
			x, err1 := strconv.Atoi(a)
			y, err2 := strconv.Atoi(b)
			if !ok || err1 != nil || err2 != nil {
				return false
			}
			left[[2]int{x, y}]++
		}
	}
	take := func(k [2]int) bool {
		if left[k] == 0 {
			return false
		}
		left[k]--
		return true
	}
	for _, m := range f.methods {
		if !m.endless {
			if !take([2]int{m.start, m.end}) {
				return false
			}
			continue
		}
		found := false
		for k, n := range left {
			if n > 0 && k[0] == m.start {
				found = take(k)
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, n := range left {
		if n != 0 {
			return false
		}
	}
	return true
}

// ParseCheck compiles the base and the head of each file with Ruby exe (docs/dynamic-layer.md §10.2): a head that does not
// compile, or whose parse-time warnings differ from the base's, fails when the file is loaded, whatever method changed; and
// the methods Ruby compiles (def and end lines) must be exactly those the body guard found, at base and head, or the guard's
// method bodies cannot be trusted. It returns "" when every file passes, otherwise the reason (Ruby that cannot run is a
// reason too).
func ParseCheck(ctx context.Context, exe string, files []ParseFile) string {
	if len(files) == 0 {
		return ""
	}
	dir, err := os.MkdirTemp("", "bgraph-rbparse-")
	if err != nil {
		return "cannot compile the changed files: " + err.Error()
	}
	defer os.RemoveAll(dir)
	args := []string{"--disable-gems", "-e", parseScript, "--"}
	for i, f := range files {
		b := filepath.Join(dir, fmt.Sprintf("%d-base.rb", i))
		h := filepath.Join(dir, fmt.Sprintf("%d-head.rb", i))
		if os.WriteFile(b, f.Base, 0o644) != nil || os.WriteFile(h, f.Head, 0o644) != nil {
			return "cannot compile the changed files: cannot write temporary files"
		}
		args = append(args, f.Path, b, h)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = withoutRubyEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("cannot compile the changed files with %s: %v %s", exe, err, strings.TrimSpace(errb.String()))
	}
	ok := 0
	for _, l := range strings.Split(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "ERR "):
			return strings.TrimPrefix(l, "ERR ")
		case strings.HasPrefix(l, "OK "):
			parts := strings.Split(strings.TrimPrefix(l, "OK "), "|")
			if ok >= len(files) || len(parts) != 3 || parts[0] != files[ok].Path {
				return "unexpected output of the compile check: " + l
			}
			f := files[ok]
			if !sameDefs(f.Base, parts[1]) || !sameDefs(f.Head, parts[2]) {
				return f.Path + ": the guard's view of the methods (def and end lines) differs from Ruby's: its block structure cannot be trusted"
			}
			ok++
		}
	}
	if ok != len(files) {
		return fmt.Sprintf("compiling the changed files gave %d results for %d files", ok, len(files))
	}
	return ""
}

// ParseFiles returns the files ParseCheck must compile: every changed file the body guard accepted (KindMethods).
func ParseFiles(changed []Changed, base, head tree.Tree) ([]ParseFile, string) {
	var out []ParseFile
	for _, c := range changed {
		if c.Kind != KindMethods {
			continue
		}
		b, err1 := base.Read(c.Path)
		h, err2 := head.Read(c.Path)
		if err1 != nil || err2 != nil {
			return nil, c.Path + ": cannot read both versions"
		}
		out = append(out, ParseFile{Path: c.Path, Base: b, Head: h})
	}
	return out, ""
}
