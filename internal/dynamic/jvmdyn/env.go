// Package jvmdyn is the JVM part of the dynamic layer (docs/dynamic.md §9): per-test-class records made by a java agent in one
// fresh test JVM per class, the guards that decide whether a selection may be narrowed below the module, and the narrowing
// itself (a selected Maven module / Gradle project runs only the test classes whose records reach a changed file).
package jvmdyn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// Eco is the ecosystem name of the records.
const Eco = "jvm"

// Collector returns the collector ID (docs/dynamic.md §9.3); trust_native_env changes it, as for Python and Node. Version 2:
// class records list their external artifacts (records without them are not usable).
func Collector(trust bool) string {
	if trust {
		return "jvm-agent-2+trust-native"
	}
	return "jvm-agent-2"
}

// JavaHome resolves the JDK the builds run with: JAVA_HOME from env (KEY=VALUE list, later entries win) or the process
// environment, else the home of `java` on PATH.
func JavaHome(env []string) (string, error) {
	if h := lookupEnv(env, "JAVA_HOME"); h != "" {
		return filepath.Clean(h), nil
	}
	exe, err := exec.LookPath("java")
	if err != nil {
		return "", errors.New("no JAVA_HOME and no java on PATH")
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(filepath.Dir(exe)), nil
}

func lookupEnv(env []string, key string) string {
	v := ""
	found := false
	for _, kv := range env {
		k, val, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, key) {
			v, found = val, true
		}
	}
	if found {
		return v
	}
	return os.Getenv(key)
}

// BuildFile reports whether repo path p is part of the build definition fingerprinted by Fingerprint: POMs, .mvn/**, Gradle
// scripts, settings, properties and version catalogs, the wrappers' properties, buildSrc/** and build-logic/**.
func BuildFile(p string) bool {
	base := path.Base(p)
	q := "/" + p
	switch {
	case strings.Contains(q, "/target/") || strings.Contains(q, "/build/") && !strings.Contains(q, "/buildSrc/"):
		return strings.HasPrefix(p, "buildSrc/") || strings.HasPrefix(p, "build-logic/")
	case base == "pom.xml", strings.HasSuffix(base, ".gradle"), strings.HasSuffix(base, ".gradle.kts"),
		base == "gradle.properties", strings.HasSuffix(base, ".versions.toml"):
		return true
	case strings.Contains(q, "/.mvn/"), strings.HasPrefix(p, "buildSrc/"), strings.HasPrefix(p, "build-logic/"):
		return true
	case base == "gradle-wrapper.properties", base == "maven-wrapper.properties":
		return true
	}
	return false
}

// Fingerprint is the JVM environment fingerprint (docs/dynamic.md §9.3): the collector, OS and architecture, the JDK's
// `release` file, the build tool found outside the repository (when no wrapper pins it) and the content of every build
// definition file of t.
func Fingerprint(collector, javaHome string, env []string, t tree.Tree) string {
	interp := []string{"os " + runtime.GOOS, "arch " + runtime.GOARCH, "jdk " + jdkRelease(javaHome)}
	files := map[string]string{}
	hasMvnw, hasGradlew := false, false
	usesMaven, usesGradle := false, false
	for _, p := range t.Files() {
		if !BuildFile(p) {
			continue
		}
		files[p] = t.Key(p)
		switch b := path.Base(p); {
		case b == "maven-wrapper.properties":
			hasMvnw = true
		case b == "gradle-wrapper.properties":
			hasGradlew = true
		case b == "pom.xml":
			usesMaven = true
		default:
			usesGradle = true
		}
	}
	if usesMaven && !hasMvnw {
		interp = append(interp, "maven "+toolVersion(env, "MAVEN_HOME", "mvn", "maven-core-"))
	}
	if usesGradle && !hasGradlew {
		interp = append(interp, "gradle "+toolVersion(env, "GRADLE_HOME", "gradle", "gradle-core-api-"))
	}
	return dynamic.Fingerprint(collector, interp, files)
}

// jdkRelease digests the JDK's release file (version, vendor, modules); "none" when there is none.
func jdkRelease(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "release"))
	if err != nil {
		return "none"
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// toolVersion names the installed build tool by the versioned core jar of its installation (homeVar, or the executable on
// PATH): "unknown" when it cannot be found.
func toolVersion(env []string, homeVar, exe, jarPrefix string) string {
	home := lookupEnv(env, homeVar)
	if home == "" {
		p, err := exec.LookPath(exe)
		if err != nil {
			return "unknown"
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		home = filepath.Dir(filepath.Dir(p))
	}
	ents, err := os.ReadDir(filepath.Join(home, "lib"))
	if err != nil {
		return "unknown"
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), jarPrefix) && strings.HasSuffix(e.Name(), ".jar") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "unknown"
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// Lombok returns why the repository is not narrowed because of Lombok (docs/dynamic.md §9.2): a build file that mentions
// lombok or a lombok.config file. Lombok rewrites declarations (it can create constants) where the lexical guard cannot see
// them. The check is repository-wide (a changed file of a Lombok module reaches the tests of every downstream module).
func Lombok(t tree.Tree) string {
	for _, p := range t.Files() {
		if path.Base(p) == "lombok.config" {
			return "Lombok configuration " + p
		}
		if !BuildFile(p) {
			continue
		}
		if b, err := t.Read(p); err == nil && strings.Contains(strings.ToLower(string(b)), "lombok") {
			return "Lombok in build file " + p
		}
	}
	return ""
}
