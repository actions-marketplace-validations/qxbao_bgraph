#!/usr/bin/env python3
"""Download and normalize T1 data for the Go ecosystem (standard library only).

Same schema and path convention as fetch_rust.py / fetch_jvm.py, pinned to the revisions in meta.json:
    - SWE-bench/SWE-bench_Multilingual (datasets-server rows API), keeping Go repos.
    - ByteDance-Seed/Multi-SWE-bench go/ directory (JSONL streamed, raw files not stored).
    - SWE-bench-Live/MultiLang split "go" (datasets-server rows API); its FAIL_TO_PASS names are "<import path>/<TestName>".
Output: tasks-go.jsonl (+ f2p_kind, lang) and adds keys go_* to meta.json (existing keys are kept).
The two Multilingual-style revisions that are not yet in meta.json are pinned below and then recorded.
Tasks present in several datasets (same instance id, or same repo + base commit across datasets) are kept once, preference order
Multi-SWE-bench, SWE-bench_Multilingual, SWE-bench-Live_MultiLang.

  python fetch_go.py DATADIR      # DATADIR must already contain meta.json
"""
import json, sys, os, time, urllib.request, urllib.parse, collections

ML = "SWE-bench/SWE-bench_Multilingual"
MS = "ByteDance-Seed/Multi-SWE-bench"
LIVE = "SWE-bench-Live/MultiLang"
LIVE_REV = "eb09c2e5db6e4c5a95e01c903c8fd1d5da33c4f3"  # same pin as dotnet_datasets
SRC = {ML: "SWE-bench_Multilingual", MS: "Multi-SWE-bench", LIVE: "SWE-bench-Live_MultiLang"}
# Multilingual lower-cases some repo names; one canonical spelling (the GitHub one) so one repo is one repo everywhere.
CANON = {"vmware-tanzu/velero": "velero-io/velero"}  # renamed upstream; Live has both spellings


def get(url, tries=6):
    for i in range(tries):
        try:
            return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=300)
        except Exception:
            if i == tries - 1:
                raise
            time.sleep(2 * (i + 1))


def rows_api(ds, split, rev):
    off = 0
    while True:
        u = (f"https://datasets-server.huggingface.co/rows?dataset={ds}&config=default&split={split}"
             f"&offset={off}&length=100&revision={rev}")
        d = json.load(get(u))
        for r in d["rows"]:
            yield r["row"]
        off += 100
        if off >= d["num_rows_total"]:
            return


def canon(repo):
    return CANON.get(repo.lower(), repo)


def multilingual(out, rev):
    # The Multilingual dataset has no language column; Go repos are recognised by a Go source file in the test patch.
    for r in rows_api(ML, "test", rev):
        files = [l.split(" b/")[-1] for l in r["test_patch"].splitlines() if l.startswith("diff --git ")]
        if not any(f.endswith("_test.go") for f in files):
            continue
        out.append({"instance_id": r["instance_id"], "repo": canon(r["repo"]), "base_commit": r["base_commit"],
                    "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                    "source": SRC[ML], "f2p_kind": "f2p", "lang": "go"})


def multiswe(out, rev):
    files = json.load(get(f"https://huggingface.co/api/datasets/{MS}/tree/{rev}/go"))
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
                # f2p_tests, else n2p_tests ("none to pass": tests absent before the fix; often the whole suite). f2p_kind records which.
                f2p = r.get("f2p_tests") or {}
                kind = "f2p"
                if not f2p and r.get("n2p_tests"):
                    f2p, kind = r["n2p_tests"], "n2p"
                repo = f'{r["org"]}/{r["repo"]}'
                out.append({"instance_id": r.get("instance_id") or f'{r["org"]}__{r["repo"]}-{r["number"]}',
                            "repo": canon(repo), "base_commit": r["base"]["sha"],
                            "patch": r.get("fix_patch", ""), "test_patch": r.get("test_patch", ""),
                            "FAIL_TO_PASS": list(f2p.keys()) if isinstance(f2p, dict) else f2p,
                            "source": SRC[MS], "f2p_kind": kind, "lang": "go"})
        print(p, flush=True)


def live(out, rev):
    for r in rows_api(LIVE, "go", rev):
        out.append({"instance_id": r["instance_id"], "repo": canon(r["repo"]), "base_commit": r["base_commit"],
                    "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                    "source": SRC[LIVE], "f2p_kind": "f2p", "lang": "go"})


def empty(f2p):
    s = f2p.strip() if isinstance(f2p, str) else json.dumps(f2p)
    return s in ("", "[]")


if __name__ == "__main__":
    d = sys.argv[1]
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    ml, ms, lv = [], [], []
    multilingual(ml, meta[ML])
    multiswe(ms, meta[MS])
    live(lv, LIVE_REV)
    # Canonical repo spelling: the first spelling seen (Multi-SWE-bench first) per lower-cased name.
    spell = {}
    for r in ms + ml + lv:
        spell.setdefault(r["repo"].lower(), r["repo"])
        r["repo"] = spell[r["repo"].lower()]
    seen_id, seen_base, rows, counts, dups, noF2P = set(), set(), [], collections.Counter(), [], []
    for r in ms + ml + lv:
        key = (r["repo"].lower(), r["base_commit"])
        if r["instance_id"].lower() in seen_id or (r["source"] != SRC[MS] and key in seen_base):
            dups.append(r["instance_id"])
            continue
        if empty(r["FAIL_TO_PASS"]):
            noF2P.append(r["instance_id"])
            continue
        seen_id.add(r["instance_id"].lower())
        seen_base.add(key)
        rows.append(r)
        counts[(r["source"], r["repo"])] += 1
    with open(os.path.join(d, "tasks-go.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["go_tasks_file"] = "tasks-go.jsonl"
    meta["go_datasets"] = {ML: meta[ML], MS: meta[MS], LIVE: LIVE_REV}
    meta["go_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["go_total"] = len(rows)
    meta["go_deduped"] = sorted(dups)
    meta["go_dropped_empty_f2p"] = sorted(noF2P)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:26} {r:40} {n}")
    print("total", len(rows), "deduped", len(dups), "empty-f2p", len(noF2P))
