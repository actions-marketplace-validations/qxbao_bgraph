#!/usr/bin/env python3
"""Collect native CTest and Meson metadata in network-disabled Docker containers."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import uuid

CMAKE_FLAGS = {
    "catchorg/Catch2": ["-DCATCH_BUILD_TESTING=ON", "-DCATCH_BUILD_EXTRA_TESTS=OFF", "-DCATCH_DEVELOPMENT_BUILD=ON"],
    "facebook/zstd": ["-DZSTD_BUILD_TESTS=ON", "-DZSTD_BUILD_STATIC=ON"],
    "fmtlib/fmt": ["-DFMT_TEST=ON", "-DFMT_DOC=OFF"],
    "nlohmann/json": ["-DJSON_BuildTests=ON"],
    "simdjson/simdjson": ["-DSIMDJSON_DEVELOPER_MODE=ON", "-DSIMDJSON_GOOGLE_BENCHMARKS=OFF", "-DSIMDJSON_COMPETITION=OFF"],
    "yhirose/cpp-httplib": ["-DHTTPLIB_TEST=ON"],
}
MESON_FLAGS = {"yhirose/cpp-httplib": ["-Dcpp-httplib_test=true"]}
CMAKE = """set -eu
mkdir -p /tmp/source /tmp/build/.cmake/api/v1/query
tar --no-same-owner -xf /input/source.tar -C /tmp/source
touch /tmp/build/.cmake/api/v1/query/codemodel-v2
project=$1
shift
if [ "${1:-}" = "-DHTTPLIB_TEST=ON" ] && [ -d /usr/src/googletest ]; then
    set -- "$@" -DFETCHCONTENT_SOURCE_DIR_GTEST=/usr/src/googletest
fi
printf '%s\n' "$@" > /out/effective-flags.txt
cmake -S "/tmp/source/$project" -B /tmp/build -G Ninja -DBUILD_TESTING=ON "$@"
if [ "$BUILD_CMAKE" = "1" ]; then
    cmake --build /tmp/build --parallel 2
fi
ctest --test-dir /tmp/build --show-only=json-v1 > /out/tests.json
mkdir -p /out/build/.cmake/api/v1
cp -R /tmp/build/.cmake/api/v1/reply /out/build/.cmake/api/v1/
"""
MESON = """set -eu
mkdir -p /tmp/source
tar --no-same-owner -xf /input/source.tar -C /tmp/source
project=$1
shift
if grep -Fq "'build_tests'" "/tmp/source/$project/meson_options.txt" 2>/dev/null; then
    set -- "$@" -Dbuild_tests=true
elif grep -Fq "'bin_tests'" "/tmp/source/$project/meson_options.txt" 2>/dev/null; then
    set -- "$@" -Dbin_tests=true
fi
printf '%s\n' "$@" > /out/effective-flags.txt
meson setup --wrap-mode=nodownload /tmp/build "/tmp/source/$project" "$@"
meson introspect --tests /tmp/build > /out/tests.json
meson introspect --targets /tmp/build > /out/targets.json
"""


def roots(repo, files):
    if repo == "facebook/zstd":
        candidates = [
            ("cmake", "build/cmake"),
            ("meson", "build/meson"),
            ("meson", "contrib/meson"),
        ]
    else:
        candidates = [("cmake", "."), ("meson", ".")]
    for system, root in candidates:
        manifest = "CMakeLists.txt" if system == "cmake" else "meson.build"
        if str(Path(root) / manifest) in files:
            yield system, root


def git(repo, *args, **kwargs):
    return subprocess.run(
        ["git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-C", str(repo), *args],
        check=True, **kwargs,
    )


def cxxopts_revision(checkout, clone, commit):
    tree = git(clone, "ls-tree", commit, "dependencies/cxxopts", capture_output=True, text=True).stdout
    if not tree:
        return ""
    mode, typ, required = tree.split()[:3]
    if (mode, typ) != ("160000", "commit"):
        return ""
    actual = git(checkout, "rev-parse", "HEAD", capture_output=True, text=True).stdout.strip()
    dirty = git(checkout, "status", "--porcelain", "--untracked-files=all", capture_output=True, text=True).stdout
    if actual != required or dirty:
        raise ValueError(f"cxxopts checkout must be clean and at gitlink {required}, found {actual}")
    return required


def append_cxxopts(archive, checkout, revision):
    prefix = "dependencies/cxxopts/"
    data = git(checkout, "archive", "--format=tar", revision, capture_output=True).stdout
    with tarfile.open(archive, "a") as bundle, tarfile.open(fileobj=io.BytesIO(data)) as dependency:
        for entry in dependency.getmembers():
            content = dependency.extractfile(entry) if entry.isfile() else None
            entry.name = prefix + entry.name
            bundle.addfile(entry, content)
        # Historical simdjson checks for .git before trying to fetch cxxopts.
        # Record the pinned HEAD without copying host Git configuration or objects.
        head = (revision + "\n").encode("ascii")
        marker = tarfile.TarInfo(prefix + ".git/HEAD")
        marker.size = len(head)
        bundle.addfile(marker, io.BytesIO(head))


def collect(args, repo, commit):
    repo_key = repo.replace("/", "__")
    clone = args.work / "repos" / repo_key
    commit_output = args.out / repo_key / commit
    commit_output.mkdir(parents=True, exist_ok=True)
    files = set(git(clone, "ls-tree", "-r", "--name-only", commit, capture_output=True, text=True).stdout.splitlines())
    projects = list(roots(repo, files))
    archive = commit_output / "source.tar"
    cxxopts = ""
    if repo == "simdjson/simdjson" and args.simdjson_cxxopts:
        cxxopts = cxxopts_revision(args.simdjson_cxxopts, clone, commit)
        if cxxopts:
            archive = commit_output / f"source-cxxopts-{cxxopts[:12]}.tar"
    if projects and not archive.exists():
        partial = archive.with_suffix(".partial")
        try:
            git(clone, "archive", "--format=tar", "-o", str(partial), commit)
            if cxxopts:
                append_cxxopts(partial, args.simdjson_cxxopts, cxxopts)
        except subprocess.CalledProcessError as err:
            print(f"{repo} {commit[:12]} archive: {err}", flush=True)
            return
        partial.replace(archive)
    for system, root in projects:
        dest = commit_output / f"{system}-{root.replace('/', '-')}"
        dest.mkdir(exist_ok=True)
        if (dest / "provenance.json").exists():
            continue
        container = "bgraph-cpp-provider-" + uuid.uuid4().hex[:12]
        command = [
            "docker", "run", "--rm", "--name", container,
            "--network=none", "--read-only", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--pids-limit=256", "--memory=2g",
            "--cpus=2", "--tmpfs=/tmp:rw,exec,size=1g", "--env=HOME=/tmp",
            f"--env=BUILD_CMAKE={int(args.build_cmake)}",
            "--mount", f"type=bind,src={archive},dst=/input/source.tar,readonly",
            "--mount", f"type=bind,src={dest},dst=/out",
            args.image, "sh", "-c", CMAKE if system == "cmake" else MESON, "setup", root,
            *(CMAKE_FLAGS.get(repo, []) if system == "cmake" else MESON_FLAGS.get(repo, [])),
        ]
        with (dest / "configure.log").open("wb") as log:
            try:
                run = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=180)
                error = "" if run.returncode == 0 else f"docker exited {run.returncode}"
            except subprocess.TimeoutExpired:
                subprocess.run(["docker", "rm", "-f", container], capture_output=True, check=False)
                error = "configure timed out"
        result = {"repo": repo, "commit": commit, "system": system, "project": root,
                  "image": args.image, "build_cmake": args.build_cmake}
        effective_flags = dest / "effective-flags.txt"
        if effective_flags.exists():
            result["flags"] = effective_flags.read_text(encoding="utf-8").splitlines()
        if cxxopts:
            result["cxxopts_revision"] = cxxopts
        if error:
            result["error"] = error
        (dest / "provenance.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        print(f"{repo} {commit[:12]} {system} {root} {error or 'collected'}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tasks", type=Path, required=True)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--image", default="bgraph-cpp-eval:cmake3.31-meson1.8")
    parser.add_argument("--simdjson-cxxopts", type=Path,
                        help="clean cxxopts checkout at the sampled simdjson revisions' Git link")
    parser.add_argument("--build-cmake", action="store_true",
                        help="build CMake targets before reading the CTest command metadata")
    parser.add_argument("--per-repo", type=int, default=5)
    parser.add_argument("--jobs", type=int, default=3)
    args = parser.parse_args()
    args.out = args.out.resolve()
    args.work = args.work.resolve()
    commits = {}
    with args.tasks.open(encoding="utf-8") as tasks:
        for line in tasks:
            task = json.loads(line)
            commits.setdefault(task["repo"], set()).add(task["base_commit"])
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        work = []
        for repo, revisions in sorted(commits.items()):
            sampled = sorted(revisions, key=lambda rev: hashlib.sha256(rev.encode()).digest())[:args.per_repo]
            work.extend(pool.submit(collect, args, repo, commit) for commit in sampled)
        for result in work:
            result.result()


if __name__ == "__main__":
    main()
