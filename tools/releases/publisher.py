"""Verified static-origin publication, with timestamp as the commit point."""
import json
from pathlib import Path
import subprocess
import tempfile
import urllib.error
import urllib.request

from tuf.ngclient import Updater
from repository import digest, encoded

ORIGINS = ("https://firmware.intsat.net/firmware/v1/", "https://firmware.intsat.space/firmware/v1/")

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise ValueError("firmware origin redirected a signed repository object")

def fetch(origin, path, expected=None):
    request = urllib.request.Request(origin + path, headers={"Accept-Encoding": "identity", "Cache-Control": "no-cache"})
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=30) as response:
        if response.status != 200 or response.headers.get("Content-Encoding", "identity").lower() != "identity":
            raise ValueError("origin must serve unmodified identity-encoded bytes")
        size = response.headers.get("Content-Length", "")
        if not size.isdecimal() or not 0 < int(size) <= 0x200000:
            raise ValueError("origin must supply a bounded exact Content-Length")
        content_type = response.headers.get_content_type()
        wanted = "application/json" if path.endswith(".json") else "application/octet-stream" if path.endswith(".bin") else "text/markdown"
        if content_type != wanted:
            raise ValueError(f"origin Content-Type for {path} must be {wanted}")
        data = response.read(int(size) + 1)
        if len(data) != int(size) or (expected is not None and data != expected):
            raise ValueError(f"public bytes differ for {path}")
        return data

def upload(command, path, data, previous_timestamp=None):
    header = {"path": path, "length": len(data), "sha256": digest(data), "previous_timestamp": previous_timestamp}
    result = subprocess.run(command, input=encoded(header) + b"\n" + data, stdout=subprocess.PIPE, check=True, timeout=120)
    receipt = json.loads(result.stdout)
    if receipt.get("sha256") != header["sha256"]:
        raise ValueError("publisher receipt does not match the uploaded object")

def verify_public(bootstrap, paths):
    for origin in ORIGINS:
        with tempfile.TemporaryDirectory(prefix="navlisten-public-check-") as temporary:
            updater = Updater(temporary, origin + "metadata/", target_base_url=origin + "targets/", bootstrap=bootstrap)
            updater.refresh()
            for path in paths:
                info = updater.get_targetinfo(path)
                if info is None:
                    raise ValueError(f"public TUF repository cannot resolve {path}")
                updater.download_target(info, str(Path(temporary) / "verified-target"))

def publish(command, files, previous_timestamp, bootstrap, targets, checkpoint=lambda: None):
    for path, data in sorted(files.items()):
        if path == "metadata/timestamp.json":
            continue
        upload(command, path, data)
        for origin in ORIGINS:
            fetch(origin, path, data)
    # Repeatable after a lost receipt: the adapter accepts byte-identical data.
    upload(command, "metadata/timestamp.json", files["metadata/timestamp.json"], previous_timestamp)
    checkpoint()
    for origin in ORIGINS:
        fetch(origin, "metadata/timestamp.json", files["metadata/timestamp.json"])
    verify_public(bootstrap, targets)
