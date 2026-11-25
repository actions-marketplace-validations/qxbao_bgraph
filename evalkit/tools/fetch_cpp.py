#!/usr/bin/env python3
"""Normalize pinned C/C++ T1 data.

    uv run --with pyarrow python evalkit/tools/fetch_cpp.py DATADIR

Keep the original shards and preserve existing meta.json keys.
"""

import argparse
import collections
import json
from pathlib import Path
from urllib.request import urlopen

import pyarrow.parquet as parquet

ML = "SWE-bench/SWE-bench_Multilingual"
MS = "ByteDance-Seed/Multi-SWE-bench"
C = "SWE-bench-c/SWE-bench-c"
REVISIONS = {
    ML: "846e647b9f33c0b51b739d005d13d85493c9af09",
    MS: "56ff018c04a38e27ada1e9d0a6d5839a51f88f0d",
    C: "417f0208132e16abf5b61320980e5dd9b2c26ae6",
}
REPOS = {
    repo.lower(): repo
    for repo in (
        "catchorg/Catch2",
        "facebook/zstd",
        "fmtlib/fmt",
        "nlohmann/json",
        "simdjson/simdjson",
        "yhirose/cpp-httplib",
    )
}


def fetch(directory, dataset, source, filename):
    destination = directory / filename
    if not destination.exists():
        url = f"https://huggingface.co/datasets/{dataset}/resolve/{REVISIONS[dataset]}/{source}"
        with urlopen(url, timeout=300) as response:
            data = response.read()
        temp = destination.with_suffix(destination.suffix + ".part")
        temp.write_bytes(data)
        temp.replace(destination)
    return destination


def parquet_rows(directory, dataset, source, filename):
    shard = fetch(directory, dataset, source, filename)
    for row in parquet.read_table(shard).to_pylist():
        repo = REPOS.get(row["repo"].lower())
        if repo is None:
            continue
        names = row["FAIL_TO_PASS"]
        yield {
            "instance_id": row["instance_id"],
            "repo": repo,
            "base_commit": row["base_commit"],
            "patch": row["patch"],
            "test_patch": row["test_patch"],
            "FAIL_TO_PASS": json.loads(names) if isinstance(names, str) else names,
            "source": dataset.rsplit("/", 1)[-1],
            "f2p_kind": "f2p",
        }


def multiswe_rows(directory):
    for repo in REPOS.values():
        filename = repo.replace("/", "__") + "_dataset.jsonl"
        language = "c" if repo == "facebook/zstd" else "cpp"
        with fetch(directory, MS, f"{language}/{filename}", filename).open(encoding="utf-8") as shard:
            for line in shard:
                row = json.loads(line)
                names = row.get("f2p_tests")
                kind = "f2p"
                if not names:
                    names = row.get("n2p_tests") or []
                    kind = "n2p"
                yield {
                    "instance_id": row["instance_id"],
                    "repo": repo,
                    "base_commit": row["base"]["sha"],
                    "patch": row.get("fix_patch"),
                    "test_patch": row.get("test_patch"),
                    "FAIL_TO_PASS": sorted(names),
                    "source": "Multi-SWE-bench",
                    "f2p_kind": kind,
                }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("datadir", type=Path)
    directory = parser.parse_args().datadir
    directory.mkdir(parents=True, exist_ok=True)
    rows = {}
    excluded = collections.defaultdict(list)
    # Prefer recorded native failure results when the same pull request appears twice.
    sources = (
        parquet_rows(directory, ML, "data/test-00000-of-00001.parquet", "ml.parquet"),
        parquet_rows(directory, C, "data/test.parquet", "swe-c.parquet"),
        multiswe_rows(directory),
    )
    for source in sources:
        for row in source:
            task_id = row["instance_id"]
            key = task_id.lower()
            if key in rows:
                excluded["duplicates"].append(task_id)
            elif not row["FAIL_TO_PASS"] or not row["patch"] or not row["test_patch"]:
                excluded["empty_patch_or_results"].append(task_id)
            else:
                rows[key] = row
    with (directory / "tasks-cpp.jsonl").open("w", encoding="utf-8", newline="\n") as output:
        for key in sorted(rows):
            output.write(json.dumps(rows[key], ensure_ascii=False) + "\n")
    meta_path = directory / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    counts = collections.Counter(f"{r['source']}|{r['repo']}" for r in rows.values())
    meta.update(
        cpp_tasks_file="tasks-cpp.jsonl",
        cpp_datasets=REVISIONS,
        cpp_counts=dict(sorted(counts.items())),
        cpp_total=len(rows),
        cpp_excluded={reason: sorted(ids) for reason, ids in sorted(excluded.items())},
    )
    meta_path.write_text(json.dumps(meta, indent=1) + "\n", encoding="utf-8")
    for key, count in sorted(counts.items()):
        print(f"{key:42} {count}")
    print(f"{len(rows)} C/C++ tasks")


if __name__ == "__main__":
    main()
