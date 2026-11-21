#!/usr/bin/env python3
"""Download and normalize T1 data for the .NET ecosystem (C#; standard library only).

Same schema and path convention as fetch_rust.py / fetch_jvm.py. Sources (revisions pinned here, recorded in meta.json):
    - microsoft/SWE-Sharp-Bench           swe-sharp-bench.csv (150 tasks, 17 repos, SWE-bench fields; FAIL_TO_PASS is a Python list literal)
    - SWE-bench-Live/MultiLang            split "cs" (136 tasks, 41 repos; datasets-server rows API)
    - SWE-bench-Live/Windows              split "test", rows with language == "C#" (a handful)
Output: tasks-dotnet.jsonl (+ f2p_kind = "f2p"), and adds keys dotnet_* to meta.json (existing keys are kept).
Dedupe: by instance_id (case-insensitive) and by (repo, base_commit); preference order SWE-Sharp-Bench, Live/MultiLang, Live/Windows.
Tasks with an empty FAIL_TO_PASS are dropped (nothing to locate) and listed in meta.json (dotnet_dropped_empty_f2p).

  python fetch_dotnet.py DATADIR      # DATADIR must already contain meta.json
"""
import csv, io, json, sys, os, time, urllib.request, collections

SHARP = "microsoft/SWE-Sharp-Bench"
LIVE = "SWE-bench-Live/MultiLang"
WIN = "SWE-bench-Live/Windows"
# Revisions pinned on 2026-11-22.
REVS = {SHARP: "41e599424ff967e0f44d69750f9f023a89f7758d",
        LIVE: "eb09c2e5db6e4c5a95e01c903c8fd1d5da33c4f3",
        WIN: "45c30d227055e19ab2e52a1ed8dd899e1f5bfa67"}
SRC = {SHARP: "SWE-Sharp-Bench", LIVE: "SWE-bench-Live_MultiLang", WIN: "SWE-bench-Live_Windows"}
# Terminal.Gui moved from gui-cs to tui-cs: one repo, one spelling (the base commits are the same history).
CANON = {"tui-cs/terminal.gui": "gui-cs/Terminal.Gui"}


def get(url, tries=6):
    for i in range(tries):
        try:
            return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=300)
        except Exception:
            if i == tries - 1:
                raise
            time.sleep(2 * (i + 1))


def row(r, ds, f2p):
    repo = CANON.get(r["repo"].lower(), r["repo"])
    return {"instance_id": r["instance_id"], "repo": repo, "base_commit": r["base_commit"], "patch": r["patch"],
            "test_patch": r["test_patch"], "FAIL_TO_PASS": f2p, "source": SRC[ds], "f2p_kind": "f2p"}


def sharp(out):
    csv.field_size_limit(1 << 30)
    raw = get(f"https://huggingface.co/datasets/{SHARP}/resolve/{REVS[SHARP]}/swe-sharp-bench.csv").read().decode("utf-8")
    for r in csv.DictReader(io.StringIO(raw, newline="")):
        out.append(row(r, SHARP, r["FAIL_TO_PASS"]))  # Python list literal string; evalkit.ParseF2P reads it


def live(out, ds, split, lang=None):
    off = 0
    while True:
        u = (f"https://datasets-server.huggingface.co/rows?dataset={ds}&config=default&split={split}"
             f"&offset={off}&length=100&revision={REVS[ds]}")
        d = json.load(get(u))
        for r in d["rows"]:
            r = r["row"]
            if lang and r.get("language") != lang:
                continue
            out.append(row(r, ds, r["FAIL_TO_PASS"]))
        off += 100
        if off >= d["num_rows_total"]:
            return


def empty(f2p):
    s = f2p.strip() if isinstance(f2p, str) else json.dumps(f2p)
    return s in ("", "[]")


if __name__ == "__main__":
    d = sys.argv[1]
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    cand = []
    sharp(cand)
    live(cand, LIVE, "cs")
    live(cand, WIN, "test", "C#")
    seen_id, seen_base, rows, counts, dups, noF2P = set(), set(), [], collections.Counter(), [], []
    for r in cand:
        k1, k2 = r["instance_id"].lower(), (r["repo"].lower(), r["base_commit"])
        if k1 in seen_id or k2 in seen_base:
            dups.append(r["instance_id"])
            continue
        seen_id.add(k1)
        seen_base.add(k2)
        if empty(r["FAIL_TO_PASS"]):
            noF2P.append(r["instance_id"])
            continue
        rows.append(r)
        counts[(r["source"], r["repo"])] += 1
    with open(os.path.join(d, "tasks-dotnet.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["dotnet_tasks_file"] = "tasks-dotnet.jsonl"
    meta["dotnet_datasets"] = dict(REVS)
    meta["dotnet_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["dotnet_total"] = len(rows)
    meta["dotnet_deduped"] = sorted(dups)
    meta["dotnet_dropped_empty_f2p"] = sorted(noF2P)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:26} {r:40} {n}")
    print("total", len(rows), "deduped", len(dups), "empty-f2p", len(noF2P))
