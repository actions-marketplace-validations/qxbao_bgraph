package evalkit

import (
	"os"
	"path/filepath"
	"runtime"
)

// Default locations of the evaluation data and tools. Nothing here is machine-specific: every default comes from the environment.
//
//	BGRAPH_EVAL_DIR    root of the evaluation data (T2 sets, reports, scratch); default <user cache dir>/bgraph-eval
//	BGRAPH_EVAL_TOOLS  optional directory of portable tools laid out as jdk17, jdk21, maven, m2, gradle-home, tmp, nuget,
//	                   dotnet, dotnet-tools, cargo-home, pnpm-store, pnpm-cache, pnpm-home, npm-cache. When it is unset the
//	                   tools come from the usual variables (JAVA_HOME, JAVA21_HOME, MAVEN_HOME, GRADLE_USER_HOME, DOTNET_ROOT,
//	                   CARGO_HOME, NUGET_PACKAGES) and PATH.

// EvalDir returns $BGRAPH_EVAL_DIR, else <user cache dir>/bgraph-eval (the temp directory when there is no cache dir).
func EvalDir() string {
	if d := os.Getenv("BGRAPH_EVAL_DIR"); d != "" {
		return d
	}
	if c, err := os.UserCacheDir(); err == nil {
		return filepath.Join(c, "bgraph-eval")
	}
	return filepath.Join(os.TempDir(), "bgraph-eval")
}

// ToolsDir returns $BGRAPH_EVAL_TOOLS ("" when unset).
func ToolsDir() string { return os.Getenv("BGRAPH_EVAL_TOOLS") }

// ToolPath returns <BGRAPH_EVAL_TOOLS>/<name> when BGRAPH_EVAL_TOOLS is set, else fallback.
func ToolPath(name, fallback string) string {
	if t := ToolsDir(); t != "" {
		return filepath.Join(t, name)
	}
	return fallback
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func homeDir(sub ...string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{h}, sub...)...)
}

// DefaultJVMTools returns the JVM tools of the environment (see the package comment of this file).
func DefaultJVMTools() JVMTools {
	if t := ToolsDir(); t != "" {
		return JVMTools{JDK17: filepath.Join(t, "jdk17"), JDK21: filepath.Join(t, "jdk21"), Maven: filepath.Join(t, "maven"),
			M2: filepath.Join(t, "m2"), GradleHome: filepath.Join(t, "gradle-home"), Tmp: filepath.Join(t, "tmp")}
	}
	return JVMTools{
		JDK17:      os.Getenv("JAVA_HOME"),
		JDK21:      firstEnv("JAVA21_HOME", "JAVA_HOME"),
		Maven:      firstEnv("MAVEN_HOME", "M2_HOME"),
		M2:         homeDir(".m2", "repository"),
		GradleHome: firstEnv("GRADLE_USER_HOME"),
		Tmp:        filepath.Join(os.TempDir(), "bgraph-eval-jvm"),
	}
}

// DefaultDotnetTools returns the .NET tools of the environment: $BGRAPH_EVAL_TOOLS/dotnet, else $DOTNET_ROOT, else dotnet on PATH;
// dotnet-affected from $BGRAPH_EVAL_TOOLS/dotnet-tools, else PATH.
func DefaultDotnetTools() DotnetTools {
	if t := ToolsDir(); t != "" {
		root := filepath.Join(t, "dotnet")
		return DotnetTools{Dotnet: filepath.Join(root, exe("dotnet")), Affected: filepath.Join(t, "dotnet-tools", exe("dotnet-affected")), Root: root}
	}
	d := DotnetTools{Dotnet: "dotnet", Affected: "dotnet-affected", Root: os.Getenv("DOTNET_ROOT")}
	if d.Root != "" {
		d.Dotnet = filepath.Join(d.Root, exe("dotnet"))
	}
	return d
}

// mvnExe is the Maven launcher of a Maven home ("mvn" on PATH when the home is empty).
func mvnExe(home string) string {
	name := "mvn"
	if runtime.GOOS == "windows" {
		name = "mvn.cmd"
	}
	if home == "" {
		return name
	}
	return filepath.Join(home, "bin", name)
}

// setIf returns []string{k=v} when v is not empty.
func setIf(k, v string) []string {
	if v == "" {
		return nil
	}
	return []string{k + "=" + v}
}
