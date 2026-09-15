#!/usr/bin/env python3
"""Embed only an explicitly supplied public root, matching the selected profile."""
import argparse
import json
from pathlib import Path


def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError("duplicate metadata key")
        result[key] = value
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--test-only", action="store_true")
    args = parser.parse_args()
    data = args.root.read_bytes()
    if not 0 < len(data) <= 8192:
        raise ValueError("public root must fit the 8 KiB device limit")
    root = json.loads(data, object_pairs_hook=pairs)
    if root["signed"]["_type"] != "root" or root["signed"].get("x_navlisten_test", False) != args.test_only:
        raise ValueError("root does not match the production/test build profile")
    if b"PRIVATE KEY" in data or any("private" in key.get("keyval", {}) for key in root["signed"]["keys"].values()):
        raise ValueError("private material must never be embedded")
    # Byte literals avoid interpreting JSON escapes as C escapes.
    args.output.write_text("#include <stddef.h>\nconst char nvf_update_root[] = {" +
        ",".join(str(byte) for byte in data) + ",0};\nconst size_t nvf_update_root_length = sizeof nvf_update_root - 1;\n")


if __name__ == "__main__":
    main()
