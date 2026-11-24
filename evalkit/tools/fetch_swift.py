#!/usr/bin/env python3
"""Fetch the pinned Swift subset of Longtail-SWE.

    uv run --with pyarrow python evalkit/tools/fetch_swift.py DATADIR

Keep the source Parquet files in DATADIR and preserve existing meta.json keys.
"""
import argparse
import collections
import json
from pathlib import Path
from urllib.request import urlopen

import pyarrow.parquet as parquet

DATASET = "nebius/Longtail-SWE"
REVISION = "d208adff5a8d3697c83c91509052bae6ed54b986"
FIELDS = ["instance_id", "repo", "base_commit", "patch", "test_patch", "FAIL_TO_PASS"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("datadir", type=Path)
    directory = parser.parse_args().datadir
    directory.mkdir(parents=True, exist_ok=True)
    rows = {}
    counts = collections.Counter()
    duplicates = []
    dropped = []
    for split in ("hard", "easy"):
        shard = directory / f"longtail-{split}.parquet"
        if not shard.exists():
            url = f"https://huggingface.co/datasets/{DATASET}/resolve/{REVISION}/data/{split}.parquet"
            with urlopen(url, timeout=300) as response:
                shard.write_bytes(response.read())
        for row in parquet.read_table(shard, columns=FIELDS, filters=[("language", "==", "Swift")]).to_pylist():
            task_id = row["instance_id"]
            if task_id in rows:
                duplicates.append(task_id)
                continue
            if not row["FAIL_TO_PASS"]:
                dropped.append(task_id)
                continue
            row.update(source=f"Longtail-SWE/{split}", f2p_kind="f2p", lang="swift")
            rows[task_id] = row
            counts[f"{split}|{row['repo']}"] += 1
    with (directory / "tasks-swift.jsonl").open("w", encoding="utf-8") as output:
        for task_id in sorted(rows):
            output.write(json.dumps(rows[task_id], ensure_ascii=False) + "\n")
    meta_path = directory / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    meta.update(swift_tasks_file="tasks-swift.jsonl", swift_datasets={DATASET: REVISION},
                swift_counts=dict(sorted(counts.items())), swift_total=len(rows),
                swift_deduped=sorted(duplicates), swift_dropped_empty_f2p=sorted(dropped))
    meta_path.write_text(json.dumps(meta, indent=1) + "\n", encoding="utf-8")
    print(f"{len(rows)} Swift tasks from {len({row['repo'] for row in rows.values()})} repositories")


if __name__ == "__main__":
    main()
