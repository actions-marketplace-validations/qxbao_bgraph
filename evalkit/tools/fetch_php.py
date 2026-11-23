#!/usr/bin/env python3
"""Normalize BugsPHP into the T1 task schema, recreating its buggy overlay.

Usage: python3 evalkit/tools/fetch_php.py WORK
The clones and extracted metadata are reused on subsequent runs. Requires Git
and an archive reader supporting 7z (bsdtar or 7z).
"""

import argparse
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import shutil
import subprocess
from tempfile import TemporaryDirectory


DATASET = "EmInReLab/bugsPHP"
PIN = "276dc0e52f2da7172985a0cd119620c6ed700d4b"
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
GIT = ["git", "-c", "gc.auto=0", "-c", "maintenance.auto=false"]


def git(repo, *args, input_data=None, lazy=True):
    env = ENV if lazy else dict(ENV, GIT_NO_LAZY_FETCH="1")
    return subprocess.run(
        [*GIT, *args], cwd=repo, input=input_data, env=env,
        check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    ).stdout


def clone(work, repo, name, existing_only=False):
    dest = work / name
    if dest.exists():
        try:
            git(dest, "rev-parse", "--verify", "HEAD^{commit}", lazy=False)
        except subprocess.CalledProcessError:
            if existing_only:
                return None
            raise
        return dest
    if existing_only:
        return None
    with TemporaryDirectory(prefix=".php-clone-", dir=work) as staging:
        args = ("clone", "--filter=blob:none", "--no-checkout", f"https://github.com/{repo}.git", "repo")
        try:
            git(staging, *args)
        except subprocess.CalledProcessError:
            git(staging, "-c", "http.version=HTTP/1.1", *args)
        (Path(staging) / "repo").rename(dest)
    return dest


def dataset(work, data):
    repo = clone(work, DATASET, "bugsPHP")
    metadata = data / "bug_metadata.json"
    pin_file = data / "bug_metadata.revision"
    if not metadata.exists() or not pin_file.exists() or pin_file.read_text().strip() != PIN:
        git(repo, "cat-file", "-e", f"{PIN}^{{commit}}")
        archive = data / "bug_metadata.7z"
        archive.write_bytes(git(repo, "show", f"{PIN}:bug_metadata.7z"))
        tar = shutil.which("bsdtar")
        if tar:
            cmd = [tar, "-xOf", str(archive), "bug_metadata.json"]
        elif shutil.which("7z"):
            cmd = ["7z", "x", "-so", str(archive), "bug_metadata.json"]
        else:
            raise RuntimeError("install bsdtar or 7z to read BugsPHP metadata")
        metadata.write_bytes(subprocess.check_output(cmd))
        pin_file.write_text(PIN + "\n", encoding="utf-8")
    return json.loads(metadata.read_bytes())


def failures(row):
    def test_name(record):
        return record["@className"] + "::" + record["@methodName"]

    buggy = {test_name(t) for t in row["test_results"]["buggy"] if t["@status"] in ("3", "4")}
    fixed = {test_name(t) for t in row["test_results"]["fixed"] if t["@status"] == "0"}
    return sorted(buggy & fixed)


def changed_paths(repo, buggy, fixed):
    raw = git(repo, "diff", "--raw", "--no-renames", "--no-abbrev", "-z", buggy, fixed)
    parts = raw.split(b"\0")
    return [(p.decode("utf-8"), meta.split()) for meta, p in zip(parts[::2], parts[1::2])]


def prefetch(repo, oids):
    if not oids:
        return
    entries = git(repo, "cat-file", "--batch-check",
                  input_data=b"\n".join(sorted(oids)) + b"\n", lazy=False)
    missing = [line.split()[0] for line in entries.splitlines() if line.endswith(b" missing")]
    if missing:
        git(repo, "-c", "fetch.negotiationAlgorithm=noop", "fetch", "--no-tags",
            "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none",
            "--stdin", "origin", input_data=b"\n".join(missing) + b"\n")


def patch(repo, buggy, fixed, paths):
    if not paths:
        return ""
    # --binary handles Git's binary patches. A text diff must be UTF-8 to round-trip
    # through the Go task schema. Non-UTF-8 text patches are reported as exclusions.
    return git(repo, "--literal-pathspecs", "diff", "--no-renames", "--binary",
               "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/",
               buggy, fixed, "--", *paths).decode("utf-8")


def normalize_group(repos, repo_name, rows, existing_only):
    repo = clone(repos, repo_name, repo_name.replace("/", "__"), existing_only)
    instance_prefix = repo_name.replace("/", "__") + "-BugsPHP-"
    if repo is None:
        return [], {instance_prefix + str(row["bug_no"]): "clone_unavailable" for row in rows}
    candidates, dropped, output = [], {}, []
    oids = set()
    commits = {row[key] for row in rows for key in ("bug_commit_id", "fixed_commit_id")}
    result = git(repo, "cat-file", "--batch-check=%(objectname) %(objecttype)",
                 input_data=("\n".join(sorted(commits)) + "\n").encode(), lazy=False)
    available = {line.split()[0].decode() for line in result.splitlines() if line.endswith(b" commit")}
    for row in rows:
        instance = instance_prefix + str(row["bug_no"])
        buggy, fixed = row["bug_commit_id"], row["fixed_commit_id"]
        if buggy not in available or fixed not in available:
            dropped[instance] = "missing_upstream_commit"
            continue
        tests = failures(row)
        if not tests:
            dropped[instance] = "no_fail_to_pass"
            continue
        try:
            changed = changed_paths(repo, buggy, fixed)
        except UnicodeDecodeError:
            dropped[instance] = "non_utf8_path"
            continue
        declared = set(row["changed_file_paths"])
        # The BugsPHP checkout starts at fixed and restores a declared file only
        # when the buggy file exists. Restore the entire remaining fixed diff in P,
        # including its tests, test configuration and any added production files.
        gold = [p for p, m in changed if p in declared and m[0] != b":000000"]
        if not gold:
            dropped[instance] = "no_gold_patch"
            continue
        gold_set = set(gold)
        overlay = [p for p, _ in changed if p not in gold_set]
        candidates.append((instance, buggy, fixed, tests, gold, overlay))
        for _, meta in changed:
            for mode, oid in zip(meta[:2], meta[2:4]):
                if mode.lstrip(b":") not in (b"000000", b"160000"):
                    oids.add(oid)
    prefetch(repo, oids)
    for instance, buggy, fixed, tests, gold, overlay in candidates:
        try:
            gold_patch = patch(repo, buggy, fixed, gold)
            test_patch = patch(repo, buggy, fixed, overlay)
        except UnicodeDecodeError:
            dropped[instance] = "non_utf8_patch"
            continue
        output.append({
            "instance_id": instance, "repo": repo_name, "base_commit": buggy,
            "patch": gold_patch, "test_patch": test_patch, "FAIL_TO_PASS": tests,
            "source": "BugsPHP", "f2p_kind": "f2p", "lang": "php",
        })
    return output, dropped


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("work", type=Path)
    parser.add_argument("--jobs", type=int, default=4)
    parser.add_argument("--existing-clones", action="store_true",
                        help="record exclusions for repositories without complete local clones")
    args = parser.parse_args()
    work = args.work.resolve()
    data, repos = work / "data", work / "repos"
    data.mkdir(parents=True, exist_ok=True)
    repos.mkdir(parents=True, exist_ok=True)
    groups = defaultdict(list)
    for row in dataset(work, data):
        groups[row["repo_full_name"]].append(row)
    output, dropped = [], {}
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        jobs = {name: pool.submit(normalize_group, repos, name, rows, args.existing_clones)
                for name, rows in groups.items()}
        for name in sorted(jobs):
            try:
                rows, excluded = jobs[name].result()
            except subprocess.CalledProcessError as exc:
                raise RuntimeError(f"{name}: {exc.stderr.decode(errors='replace')}") from exc
            print(f"{name}: {len(rows)} normalized, {len(excluded)} excluded", flush=True)
            output.extend(rows)
            dropped.update(excluded)
    tasks_file = "tasks-php.jsonl"
    (data / tasks_file).write_text(
        "".join(json.dumps(row, ensure_ascii=False) + "\n" for row in output), encoding="utf-8",
    )
    meta_file = data / "meta.json"
    meta = json.loads(meta_file.read_bytes()) if meta_file.exists() else {}
    counts = Counter(row["repo"] for row in output)
    meta.update(php_tasks_file=tasks_file, php_datasets={DATASET: PIN}, php_total=len(output),
                php_counts=dict(sorted(counts.items())), php_excluded=dropped)
    meta_file.write_text(json.dumps(meta, indent=1) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
