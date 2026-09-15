#!/usr/bin/env python3
"""Origin adapter: receive one verified immutable object or commit timestamp last.

Run on the static origin through the operator's configured transport. This
program has no signing keys. Stdin is one JSON header line followed by bytes.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile

def write(root, header, data):
    path = header["path"]
    if not isinstance(path, str) or len(path) > 280 or not re.fullmatch(r"(?:metadata|targets)/[A-Za-z0-9._/-]+", path) or ".." in path or "//" in path:
        raise ValueError("invalid origin object path")
    if header.get("length") != len(data) or header.get("sha256") != hashlib.sha256(data).hexdigest() or not 0 < len(data) <= 0x200000:
        raise ValueError("origin object length or digest differs")
    root = root.resolve(strict=True)
    target = root / path
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.is_symlink() or not target.parent.resolve().is_relative_to(root):
        raise ValueError("origin object path escapes its root")
    with (root / ".publication.lock").open("a+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        old = target.read_bytes() if target.is_file() else None
        if old == data:
            return {"sha256": header["sha256"], "already_present": True}
        timestamp = path == "metadata/timestamp.json"
        if not timestamp and old is not None:
            raise ValueError("immutable object already exists with different bytes")
        if timestamp:
            previous = hashlib.sha256(old).hexdigest() if old is not None else None
            if header.get("previous_timestamp") != previous:
                raise ValueError("timestamp changed since this transaction began; prepare higher-version metadata")
            value = json.loads(data)
            version = value["signed"]["version"]
            if type(version) is not int or version <= 0 or (old and version <= json.loads(old)["signed"]["version"]):
                raise ValueError("timestamp version must increase")
        descriptor, name = tempfile.mkstemp(prefix=".upload-", dir=target.parent)
        temporary = Path(name)
        try:
            with os.fdopen(descriptor, "wb") as output:
                output.write(data); output.flush(); os.fsync(output.fileno())
            os.chmod(temporary, 0o644)
            if timestamp:
                os.replace(temporary, target)
            else:
                os.link(temporary, target)
            directory = os.open(target.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            temporary.unlink(missing_ok=True)
    return {"sha256": header["sha256"], "already_present": False}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    args = parser.parse_args()
    header = sys.stdin.buffer.readline(4097)
    if len(header) > 4096 or not header.endswith(b"\n"):
        raise ValueError("invalid adapter header")
    data = sys.stdin.buffer.read(0x200001)
    print(json.dumps(write(args.root, json.loads(header), data)))

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError) as error:
        raise SystemExit(str(error)) from None
