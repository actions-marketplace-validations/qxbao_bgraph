#!/usr/bin/env python3
"""Download and normalize T1 data for the Python ecosystem (standard library only).

Same schema and path convention as fetch_rust.py, pinned to a Hugging Face revision recorded in meta.json:
    - princeton-nlp/SWE-bench_Verified (datasets-server rows API, test split, 500 tasks).
Output: tasks-python.jsonl, and adds keys python_* to meta.json (existing keys are kept).

  python fetch_python.py DATADIR      # DATADIR must already contain meta.json
  python fetch_python.py DATADIR --survey   # list repo counts of other public SWE datasets (no files written)
"""
import json, sys, os, time, urllib.request, collections

VERIFIED = "princeton-nlp/SWE-bench_Verified"
# Revision pinned on 2026-11-22 (last modified 2025-02-18). Used for every request so the data cannot drift.
VERIFIED_REV = "c104f840cc67f8b6eec6f759ebc8b2693d585d4a"


def get(url, tries=6):
    for i in range(tries):
        try:
            return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=300)
        except Exception:
            if i == tries - 1:
                raise
            time.sleep(2 * (i + 1))


def rows(ds, rev, split, n, page=50, config="default"):
    for off in range(0, n, page):
        u = (f"https://datasets-server.huggingface.co/rows?dataset={ds}"
             f"&config={config}&split={split}&offset={off}&length={page}&revision={rev}")
        for r in json.load(get(u))["rows"]:
            yield r["row"]


def verified(out, counts):
    for r in rows(VERIFIED, VERIFIED_REV, "test", 500):
        out.append({"instance_id": r["instance_id"], "repo": r["repo"], "base_commit": r["base_commit"],
                    "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                    "source": "SWE-bench_Verified", "f2p_kind": "f2p"})
        counts[("SWE-bench_Verified", r["repo"])] += 1


def survey():
    """Repo counts of other public datasets that have the SWE-bench task fields."""
    specs = [("princeton-nlp/SWE-bench", "test", 2294), ("SWE-Gym/SWE-Gym", "train", 2438)]
    for ds, split, n in specs:
        c = collections.Counter()
        fields = None
        for r in rows(ds, "main", split, n):
            fields = fields or sorted(r)
            c[r["repo"]] += 1
        print(f"== {ds} {split} n={sum(c.values())} fields={fields}")
        for k, v in c.most_common():
            print(f"  {v:5d} {k}")


if __name__ == "__main__":
    d = sys.argv[1]
    if "--survey" in sys.argv:
        survey()
        sys.exit(0)
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    out, counts = [], collections.Counter()
    verified(out, counts)
    with open(os.path.join(d, "tasks-python.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in out:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["python_tasks_file"] = "tasks-python.jsonl"
    meta["python_datasets"] = {VERIFIED: VERIFIED_REV}
    meta["python_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["python_total"] = len(out)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:20} {r:32} {n}")
    print("total", len(out))
