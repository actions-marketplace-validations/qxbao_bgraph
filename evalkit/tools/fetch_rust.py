#!/usr/bin/env python3
"""Download and normalize T1 data for the Rust ecosystem (standard library only).

Same schema and path as fetch_datasets.py (Node), but for Rust and pinned to the revisions in meta.json:
    - SWE-bench/SWE-bench_Multilingual (datasets-server rows API), keeping Rust repos.
    - ByteDance-Seed/Multi-SWE-bench rust/ directory (JSONL downloaded directly, streamed, raw files not stored).
Output: tasks-rust.jsonl (+ f2p_kind), and adds keys rust_* to meta.json (existing keys are kept).

  python fetch_rust.py DATADIR      # DATADIR must already contain meta.json from fetch_datasets.py
"""
import json, sys, os, urllib.request, urllib.parse, collections

ML = "SWE-bench/SWE-bench_Multilingual"
MS = "ByteDance-Seed/Multi-SWE-bench"
# Multilingual lower-cases some repo names; use the Multi-SWE-bench spelling so one repo is one repo everywhere.
CANON = {"burntsushi/ripgrep": "BurntSushi/ripgrep"}
ML_RUST = {"tokio-rs/tokio", "sharkdp/bat", "tokio-rs/axum", "nushell/nushell", "uutils/coreutils", "burntsushi/ripgrep"}


def get(url):
    return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=300)


def multilingual(out, rev, counts):
    for off in (0, 100, 200):
        u = (f"https://datasets-server.huggingface.co/rows?dataset={ML}"
             f"&config=default&split=test&offset={off}&length=100&revision={rev}")
        for r in json.load(get(u))["rows"]:
            r = r["row"]
            if r["repo"].lower() not in ML_RUST:
                continue
            repo = CANON.get(r["repo"].lower(), r["repo"])
            out.append({"instance_id": r["instance_id"], "repo": repo, "base_commit": r["base_commit"],
                        "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                        "source": "SWE-bench_Multilingual", "f2p_kind": "f2p"})
            counts[("SWE-bench_Multilingual", repo)] += 1


def multiswe(out, rev, counts):
    files = json.load(get(f"https://huggingface.co/api/datasets/{MS}/tree/{rev}/rust"))
    for f in files:
        p = f["path"]
        if not p.endswith(".jsonl"):
            continue
        with get(f"https://huggingface.co/datasets/{MS}/resolve/{rev}/" + urllib.parse.quote(p)) as resp:
            for line in resp:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                # Rust quirk: tests added by test_patch are "none to pass" (n2p_tests), not f2p_tests; many tasks have an
                # empty f2p_tests. FAIL_TO_PASS = f2p_tests, else n2p_tests ("f2p_kind" records which; extra key, ignored by LoadTasks).
                f2p = r.get("f2p_tests") or {}
                kind = "f2p"
                if not f2p and r.get("n2p_tests"):
                    f2p, kind = r["n2p_tests"], "n2p"
                repo = f'{r["org"]}/{r["repo"]}'
                out.append({"instance_id": r.get("instance_id") or f'{r["org"]}__{r["repo"]}-{r["number"]}',
                            "repo": repo, "base_commit": r["base"]["sha"],
                            "patch": r.get("fix_patch", ""), "test_patch": r.get("test_patch", ""),
                            "FAIL_TO_PASS": list(f2p.keys()) if isinstance(f2p, dict) else f2p,
                            "source": "Multi-SWE-bench", "f2p_kind": kind})
                counts[("Multi-SWE-bench", repo)] += 1
        print(p, flush=True)


if __name__ == "__main__":
    d = sys.argv[1]
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    rows, counts = [], collections.Counter()
    multilingual(rows, meta[ML], counts)
    multiswe(rows, meta[MS], counts)
    with open(os.path.join(d, "tasks-rust.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["rust_tasks_file"] = "tasks-rust.jsonl"
    meta["rust_datasets"] = {ML: meta[ML], MS: meta[MS]}
    meta["rust_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["rust_total"] = len(rows)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:24} {r:28} {n}")
    print("total", len(rows))
