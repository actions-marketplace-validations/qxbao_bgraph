#!/usr/bin/env python3
"""Download and normalize T1 data for the Ruby ecosystem (standard library only).

Same schema and path convention as fetch_go.py, pinned to the revisions recorded in meta.json:
    - SWE-bench/SWE-bench_Multilingual (datasets-server rows API), keeping rows whose test patch touches a .rb file.
    - ByteDance-Seed/Multi-SWE-RL ruby/ directories (JSONL streamed, raw files not stored): asciidoctor, jekyll.
Surveyed on 2026-11-23 and without Ruby instances: SWE-bench-Live/MultiLang (no ruby split), Multi-SWE-bench,
Multi-SWE-bench_mini, Multi-SWE-bench-flash, nebius/SWE-rebench-V2 (20 languages, no ruby), SWE-rebench-V2.1,
SWE-PolyBench, SWE-bench_Pro, SWE-Bench-plus-plus.
Output: tasks-ruby.jsonl (+ f2p_kind, lang) and adds keys ruby_* to meta.json (existing keys are kept).
Tasks present in several datasets (same instance id, or same repo + base commit across datasets) are kept once, preference order
SWE-bench_Multilingual (curated, harness-validated), Multi-SWE-RL.

  python fetch_ruby.py DATADIR      # DATADIR must already contain meta.json
"""
import json, sys, os, time, urllib.request, urllib.parse, collections

ML = "SWE-bench/SWE-bench_Multilingual"
RL = "ByteDance-Seed/Multi-SWE-RL"
RL_REV = "9777648932daa214ba18c70c81e85821b5836f32"  # pinned 2026-11-23 (last modified 2025-07-23)
SRC = {ML: "SWE-bench_Multilingual", RL: "Multi-SWE-RL"}


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


def multilingual(out, rev):
    # No language column; Ruby repos are recognised by a .rb file in the test patch.
    for r in rows_api(ML, "test", rev):
        files = [l.split(" b/")[-1] for l in r["test_patch"].splitlines() if l.startswith("diff --git ")]
        if not any(f.endswith(".rb") for f in files):
            continue
        out.append({"instance_id": r["instance_id"], "repo": r["repo"], "base_commit": r["base_commit"],
                    "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                    "source": SRC[ML], "f2p_kind": "f2p", "lang": "ruby"})


def multiswe_rl(out, rev):
    files = json.load(get(f"https://huggingface.co/api/datasets/{RL}/tree/{rev}?recursive=true"))
    for f in files:
        p = f["path"]
        if "/ruby/" not in p or not p.endswith(".jsonl"):
            continue
        with get(f"https://huggingface.co/datasets/{RL}/resolve/{rev}/" + urllib.parse.quote(p)) as resp:
            for line in resp:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                # f2p_tests, else n2p_tests ("none to pass": tests absent before the fix). f2p_kind records which.
                f2p = r.get("f2p_tests") or {}
                kind = "f2p"
                if not f2p and r.get("n2p_tests"):
                    f2p, kind = r["n2p_tests"], "n2p"
                out.append({"instance_id": r.get("instance_id") or f'{r["org"]}__{r["repo"]}-{r["number"]}',
                            "repo": f'{r["org"]}/{r["repo"]}', "base_commit": r["base"]["sha"],
                            "patch": r.get("fix_patch", ""), "test_patch": r.get("test_patch", ""),
                            "FAIL_TO_PASS": list(f2p.keys()) if isinstance(f2p, dict) else f2p,
                            "source": SRC[RL], "f2p_kind": kind, "lang": "ruby"})
        print(p, flush=True)


def empty(f2p):
    s = f2p.strip() if isinstance(f2p, str) else json.dumps(f2p)
    return s in ("", "[]")


if __name__ == "__main__":
    d = sys.argv[1]
    mp = os.path.join(d, "meta.json")
    meta = json.load(open(mp, encoding="utf-8"))
    ml, rl = [], []
    multilingual(ml, meta[ML])
    multiswe_rl(rl, RL_REV)
    # Canonical repo spelling: the first spelling seen (Multilingual first) per lower-cased name.
    spell = {}
    for r in ml + rl:
        spell.setdefault(r["repo"].lower(), r["repo"])
        r["repo"] = spell[r["repo"].lower()]
    # Same base commit within one dataset = different pull requests on one commit: distinct tasks, both kept.
    seen_id, seen_base, rows, counts, dups, noF2P = set(), {}, [], collections.Counter(), [], []
    for r in ml + rl:
        key = (r["repo"].lower(), r["base_commit"])
        if r["instance_id"].lower() in seen_id or seen_base.get(key, r["source"]) != r["source"]:
            dups.append(r["instance_id"] + "@" + r["source"])
            continue
        if empty(r["FAIL_TO_PASS"]):
            noF2P.append(r["instance_id"])
            continue
        seen_id.add(r["instance_id"].lower())
        seen_base.setdefault(key, r["source"])
        rows.append(r)
        counts[(r["source"], r["repo"])] += 1
    with open(os.path.join(d, "tasks-ruby.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta["ruby_tasks_file"] = "tasks-ruby.jsonl"
    meta["ruby_datasets"] = {ML: meta[ML], RL: RL_REV}
    meta["ruby_counts"] = {f"{s}|{r}": n for (s, r), n in sorted(counts.items())}
    meta["ruby_total"] = len(rows)
    meta["ruby_deduped"] = sorted(dups)
    meta["ruby_dropped_empty_f2p"] = sorted(noF2P)
    with open(mp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for (s, r), n in sorted(counts.items()):
        print(f"{s:26} {r:40} {n}")
    print("total", len(rows), "deduped", len(dups), "empty-f2p", len(noF2P))
