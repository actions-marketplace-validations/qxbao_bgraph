#!/usr/bin/env python3
"""Download and normalize T1 data for the JVM ecosystem (Java + Kotlin; standard library only).

Same schema and path convention as fetch_rust.py, pinned to the revisions in meta.json:
    - SWE-bench/SWE-bench_Multilingual (datasets-server rows API), keeping JVM repos.
    - ByteDance-Seed/Multi-SWE-bench java/ and kotlin/ directories (JSONL streamed, raw files not stored).
Output: tasks-jvm.jsonl (+ f2p_kind, lang), and adds keys jvm_* to meta.json (existing keys are kept).
Tasks present in both datasets (same repo + base commit) are kept once, preferring Multi-SWE-bench (it has f2p_kind).

  python fetch_jvm.py DATADIR      # DATADIR must already contain meta.json
"""
import json, sys, os, time, urllib.request, urllib.parse, collections

ML = "SWE-bench/SWE-bench_Multilingual"
MS = "ByteDance-Seed/Multi-SWE-bench"
# Multilingual lower-cases some repo names; use one canonical spelling so one repo is one repo everywhere.
CANON = {"projectlombok/lombok": "projectlombok/lombok", "reactivex/rxjava": "ReactiveX/RxJava",
         "javaparser/javaparser": "javaparser/javaparser", "apache/lucene": "apache/lucene",
         "apache/druid": "apache/druid", "google/gson": "google/gson"}
ML_JVM = set(CANON)
MS_DIRS = ("java", "kotlin")


def get(url, tries=6):
    for i in range(tries):
        try:
            return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=300)
        except Exception:
            if i == tries - 1:
                raise
            time.sleep(2 * (i + 1))


def multilingual(out, rev):
    for off in (0, 100, 200):
        u = (f"https://datasets-server.huggingface.co/rows?dataset={ML}"
             f"&config=default&split=test&offset={off}&length=100&revision={rev}")
        for r in json.load(get(u))["rows"]:
            r = r["row"]
            if r["repo"].lower() not in ML_JVM:
                continue
            repo = CANON[r["repo"].lower()]
            out.append({"instance_id": r["instance_id"], "repo": repo, "base_commit": r["base_commit"],
                        "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                        "source": "SWE-bench_Multilingual", "f2p_kind": "f2p", "lang": "java"})


def multiswe(out, rev):
    for d in MS_DIRS:
        files = json.load(get(f"https://huggingface.co/api/datasets/{MS}/tree/{rev}/{d}"))
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
                    # Quirk: for most Java tasks f2p_tests is empty and n2p_tests ("none to pass": tests that did not run before the
                    # fix) lists every test class of the module/suite, not just the new ones. FAIL_TO_PASS = f2p_tests, else n2p_tests;
                    # f2p_kind records which, so the oracle can restrict n2p names to files the test patch touches.
                    f2p = r.get("f2p_tests") or {}
                    kind = "f2p"
                    if not f2p and r.get("n2p_tests"):
                        f2p, kind = r["n2p_tests"], "n2p"
                    repo = f'{r["org"]}/{r["repo"]}'
                    out.append({"instance_id": r.get("instance_id") or f'{r["org"]}__{r["repo"]}-{r["number"]}',
                                "repo": repo, "base_commit": r["base"]["sha"],
                                "patch": r.get("fix_patch", ""), "test_patch": r.get("test_patch", ""),
                                "FAIL_TO_PASS": list(f2p.keys()) if isinstance(f2p, dict) else f2p,
                                "source": "Multi-SWE-bench", "f2p_kind": kind, "lang": d})
            print(p, flush=True)


if __name__ == "__main__":
    d = sys.argv[1]
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    ml, ms = [], []
    multilingual(ml, meta[ML])
    multiswe(ms, meta[MS])
    # Dedupe: by instance_id everywhere; across datasets also by (repo, base commit) (two different PRs can share a base commit inside
    # one dataset, so that key is only applied to Multilingual rows against Multi-SWE-bench rows).
    seen_id, ms_base, rows, counts, dups = set(), {(r["repo"].lower(), r["base_commit"]) for r in ms}, [], collections.Counter(), []
    for r in ms + ml:  # Multi-SWE-bench first: it wins duplicates
        if r["instance_id"].lower() in seen_id or (r["source"] != "Multi-SWE-bench" and (r["repo"].lower(), r["base_commit"]) in ms_base):
            dups.append(r["instance_id"])
            continue
        seen_id.add(r["instance_id"].lower())
        rows.append(r)
        counts[(r["source"], r["repo"])] += 1
    with open(os.path.join(d, "tasks-jvm.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["jvm_tasks_file"] = "tasks-jvm.jsonl"
    meta["jvm_datasets"] = {ML: meta[ML], MS: meta[MS]}
    meta["jvm_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["jvm_total"] = len(rows)
    meta["jvm_deduped"] = sorted(dups)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:24} {r:36} {n}")
    print("total", len(rows), "deduped", len(dups), dups)
