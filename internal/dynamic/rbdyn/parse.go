package rbdyn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
    puts "OK #{name}"
  end
end
`

// ParseFile is one changed file whose changes the body guard accepted.
type ParseFile struct {
	Path       string
	Base, Head []byte
}

// ParseCheck compiles the base and the head of each file with Ruby exe (docs/dynamic-layer.md §10.2): a head that does not
// compile, or whose parse-time warnings differ from the base's, fails when the file is loaded, whatever method changed. It
// returns "" when every file passes, otherwise the reason (Ruby that cannot run is a reason too).
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
