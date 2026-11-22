package jvmdyn

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The collector agent is Java source embedded in the binary and compiled with the JDK of the build at record time (no binary
// in the repository): agent/bgraph/agent/*.java, agent/MANIFEST.MF.
//
//go:embed agent/MANIFEST.MF agent/bgraph/agent/*.java
var agentSrc embed.FS

// agentHash digests the embedded agent sources.
func agentHash() (string, []string, error) {
	var names []string
	err := fs.WalkDir(agentSrc, "agent", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	})
	if err != nil {
		return "", nil, err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, _ := agentSrc.ReadFile(n)
		fmt.Fprintf(h, "%s %d\n", n, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], names, nil
}

func jdkTool(javaHome, name string) string {
	exe := filepath.Join(javaHome, "bin", name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	return exe
}

// BuildAgent compiles the agent with the JDK in javaHome into cacheDir (reused while the sources and the JDK release are the
// same) and returns the jar path. The path must not contain spaces (it is passed through JAVA_TOOL_OPTIONS).
func BuildAgent(ctx context.Context, javaHome, cacheDir string) (string, error) {
	sum, names, err := agentHash()
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(filepath.Join(cacheDir, "jvm-agent-"+sum+"-"+jdkRelease(javaHome)))
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(dir, " \t") {
		return "", fmt.Errorf("the agent directory %q contains a space (JAVA_TOOL_OPTIONS cannot carry it)", dir)
	}
	jar := filepath.Join(dir, "bgraph-agent.jar")
	if st, err := os.Stat(jar); err == nil && st.Size() > 0 {
		return jar, nil
	}
	tmp, err := os.MkdirTemp(cacheDir, "jvm-agent-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	var srcs []string
	for _, n := range names {
		b, _ := agentSrc.ReadFile(n)
		dst := filepath.Join(tmp, "src", filepath.FromSlash(strings.TrimPrefix(n, "agent/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return "", err
		}
		if strings.HasSuffix(n, ".java") {
			srcs = append(srcs, dst)
		}
	}
	out := filepath.Join(tmp, "out")
	args := append([]string{"--release", "11", "-nowarn", "-encoding", "UTF-8", "-d", out}, srcs...)
	if b, err := exec.CommandContext(ctx, jdkTool(javaHome, "javac"), args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("javac (agent): %v: %s", err, tail(string(b), 600))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	part := jar + ".part"
	if b, err := exec.CommandContext(ctx, jdkTool(javaHome, "jar"), "cfm", part, filepath.Join(tmp, "src", "MANIFEST.MF"), "-C", out, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("jar (agent): %v: %s", err, tail(string(b), 600))
	}
	if err := os.Rename(part, jar); err != nil {
		return "", err
	}
	return jar, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
