#!/usr/bin/env python3
"""Fetch the pinned Dart instances of Longtail-SWE and SWE-rebench-V2.

    uv run --with pyarrow python evalkit/tools/fetch_dart.py DATADIR

Sources (pinned revisions; Parquet shards are kept in DATADIR/src-dart and reused):
    - nebius/Longtail-SWE, splits hard + easy, language == "Dart".
    - nebius/SWE-rebench-V2, split train, language == "dart".
Surveyed on 2026-11-24 without Dart instances: SWE-bench_Multilingual, Multi-SWE-bench (+ _mini, -flash),
Multi-SWE-RL, SWE-bench-Live/MultiLang (splits c, cpp, go, js, rust, java, ts, cs), SWE-PolyBench (TS/JS/Java/Python),
SWE-Gym and nebius/SWE-rebench (Python).

Output: tasks-dart.jsonl (common schema + source, f2p_kind, lang, test_cmd) and dart_* keys in meta.json (existing keys
are kept). Instances present in several datasets (same instance id, or same repo + base commit across datasets) are kept
once, preference order Longtail-SWE (hard, easy), SWE-rebench-V2.
"""
import argparse
import collections
import json
from pathlib import Path
from urllib.request import Request, urlopen

import pyarrow.parquet as parquet

LONGTAIL = "nebius/Longtail-SWE"
LONGTAIL_REV = "d208adff5a8d3697c83c91509052bae6ed54b986"
REBENCH = "nebius/SWE-rebench-V2"
REBENCH_REV = "d7608f017a29ab256400ad4b73d5248d97204966"  # pinned 2026-11-24 (last modified 2026-11-05)
FIELDS = ["instance_id", "repo", "base_commit", "patch", "test_patch", "FAIL_TO_PASS"]


def shard(directory, name, dataset, revision, path):
    target = directory / name
    if not target.exists():
        url = f"https://huggingface.co/datasets/{dataset}/resolve/{revision}/{path}"
        tmp = target.with_suffix(".part")
        with urlopen(Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=600) as response, tmp.open("wb") as fh:
            while True:
                chunk = response.read(1 << 20)
                if not chunk:
                    break
                fh.write(chunk)
        tmp.replace(target)
    return target


def longtail(cache):
    for split in ("hard", "easy"):
        path = shard(cache, f"longtail-{split}.parquet", LONGTAIL, LONGTAIL_REV, f"data/{split}.parquet")
        table = parquet.read_table(path, filters=[("language", "==", "Dart")])
        for row in table.to_pylist():
            out = {k: row[k] for k in FIELDS}
            out.update(source=f"Longtail-SWE/{split}", f2p_kind="f2p", lang="dart")
            out["test_cmd"] = test_cmd(row)
            yield out


def rebench(cache):
    path = shard(cache, "swe-rebench-v2-train.parquet", REBENCH, REBENCH_REV, "data/train-00000-of-00001.parquet")
    table = parquet.read_table(path, columns=FIELDS + ["install_config"], filters=[("language", "==", "dart")])
    for row in table.to_pylist():
        out = {k: row[k] for k in FIELDS}
        out.update(source="SWE-rebench-V2", f2p_kind="f2p", lang="dart")
        out["test_cmd"] = test_cmd(row)
        yield out


def test_cmd(row):
    config = row.get("install_config")
    if isinstance(config, str):
        try:
            config = json.loads(config)
        except ValueError:
            return ""
    if isinstance(config, dict):
        return config.get("test_cmd") or ""
    return ""


def empty(f2p):
    if f2p is None:
        return True
    if isinstance(f2p, str):
        return f2p.strip() in ("", "[]")
    return len(f2p) == 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("datadir", type=Path)
    directory = parser.parse_args().datadir
    cache = directory / "src-dart"
    cache.mkdir(parents=True, exist_ok=True)
    rows = list(longtail(cache)) + list(rebench(cache))
    spell = {}
    for r in rows:
        spell.setdefault(r["repo"].lower(), r["repo"])
        r["repo"] = spell[r["repo"].lower()]
        if not isinstance(r["FAIL_TO_PASS"], str):
            r["FAIL_TO_PASS"] = list(r["FAIL_TO_PASS"] or [])
    kept, seen_id, seen_base = [], set(), {}
    counts, dups, no_f2p = collections.Counter(), [], []
    for r in rows:
        family = r["source"].split("/")[0]
        key = (r["repo"].lower(), r["base_commit"])
        if r["instance_id"].lower() in seen_id or seen_base.get(key, family) != family:
            dups.append(r["instance_id"] + "@" + r["source"])
            continue
        if empty(r["FAIL_TO_PASS"]):
            no_f2p.append(r["instance_id"] + "@" + r["source"])
            continue
        seen_id.add(r["instance_id"].lower())
        seen_base.setdefault(key, family)
        kept.append(r)
        counts[f"{r['source']}|{r['repo']}"] += 1
    kept.sort(key=lambda r: r["instance_id"])
    with (directory / "tasks-dart.jsonl").open("w", encoding="utf-8", newline="\n") as fh:
        for r in kept:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    meta_path = directory / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    meta.update(dart_tasks_file="tasks-dart.jsonl", dart_datasets={LONGTAIL: LONGTAIL_REV, REBENCH: REBENCH_REV},
                dart_counts=dict(sorted(counts.items())), dart_total=len(kept),
                dart_deduped=sorted(dups), dart_dropped_empty_f2p=sorted(no_f2p))
    with meta_path.open("w", encoding="utf-8", newline="\n") as fh:
        json.dump(meta, fh, indent=1)
    for key, n in sorted(counts.items()):
        print(f"{key:70} {n}")
    print(f"total {len(kept)} from {len({r['repo'] for r in kept})} repositories; deduped {len(dups)}; empty f2p {len(no_f2p)}")


if __name__ == "__main__":
    main()
