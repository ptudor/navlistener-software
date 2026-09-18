"""Verified static-origin publication, with timestamp as the commit point."""
import json
import re
from pathlib import Path
import subprocess
import tempfile
import urllib.error
import urllib.request

from tuf.ngclient import Updater, UpdaterConfig
from tuf.ngclient.fetcher import FetcherInterface
from tuf.api.exceptions import DownloadHTTPError
from repository import digest, encoded

# Each release track is a separate repository below its own path. The firmware
# compiles in the same pair for its profile; see update_runtime.c.
ORIGINS = {
    "trusted": ("https://firmware.intsat.net/firmware/trusted/v1/", "https://firmware.intsat.space/firmware/trusted/v1/"),
    "open": ("https://firmware.intsat.net/firmware/open/v1/", "https://firmware.intsat.space/firmware/open/v1/"),
}

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise ValueError("firmware origin redirected a signed repository object")

def fetch(origin, path, expected=None):
    if not re.fullmatch(r"(?:metadata|targets)/[A-Za-z0-9._/-]+", path) or ".." in path or "//" in path:
        raise ValueError("invalid public repository object path")
    request = urllib.request.Request(origin + path, headers={"Accept-Encoding": "identity", "Cache-Control": "no-cache"})
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=30) as response:
        if response.status != 200 or response.headers.get("Content-Encoding", "identity").lower() != "identity":
            raise ValueError("origin must serve unmodified identity-encoded bytes")
        size = response.headers.get("Content-Length", "")
        if not size.isdecimal() or not 0 < int(size) <= 0x400000:
            raise ValueError("origin must supply a bounded exact Content-Length")
        content_type = response.headers.get_content_type()
        wanted = "application/json" if path.endswith(".json") else "application/octet-stream" if path.endswith(".bin") else "text/markdown"
        if content_type != wanted:
            raise ValueError(f"origin Content-Type for {path} must be {wanted}")
        data = response.read(int(size) + 1)
        if len(data) != int(size) or (expected is not None and data != expected):
            raise ValueError(f"public bytes differ for {path}")
        return data

def upload(command, track, path, data, previous_timestamp=None):
    header = {"track": track, "path": path, "length": len(data), "sha256": digest(data), "previous_timestamp": previous_timestamp}
    result = subprocess.run(command, input=encoded(header) + b"\n" + data, stdout=subprocess.PIPE, check=True, timeout=120)
    receipt = json.loads(result.stdout)
    if receipt.get("sha256") != header["sha256"]:
        raise ValueError("publisher receipt does not match the uploaded object")

class OriginFetcher(FetcherInterface):
    def __init__(self, origin):
        self.origin = origin

    def _fetch(self, url):
        if not url.startswith(self.origin):
            raise ValueError("reference verifier left its configured origin")
        try:
            yield fetch(self.origin, url[len(self.origin):])
        except urllib.error.HTTPError as error:
            error.close()
            raise DownloadHTTPError("origin request failed", error.code) from error

def verify_public(origins, bootstrap, paths, firmware=None):
    for origin in origins:
        with tempfile.TemporaryDirectory(prefix="navlisten-public-check-") as temporary:
            updater = Updater(temporary, origin + "metadata/", target_base_url=origin + "targets/", bootstrap=bootstrap,
                fetcher=OriginFetcher(origin), config=UpdaterConfig(max_root_rotations=32, max_delegations=4,
                root_max_length=8192, timestamp_max_length=4096, snapshot_max_length=8192, targets_max_length=12288))
            updater.refresh()
            def download(path):
                info = updater.get_targetinfo(path)
                if info is None:
                    raise ValueError(f"public TUF repository cannot resolve {path}")
                updater.download_target(info, str(Path(temporary) / "verified-target"))
                return (Path(temporary) / "verified-target").read_bytes()
            for path in paths:
                data = download(path)
                if path.startswith("releases/"):
                    manifest = json.loads(data)
                    image = download(manifest["artifact"])
                    if digest(image) != manifest["sha256"] or len(image) != manifest["length"]:
                        raise ValueError("public image differs from its manifest")
                    for field in ("provenance", "licenses", "notes"):
                        if digest(download(manifest[field])) != manifest[field + "_sha256"]:
                            raise ValueError("public companion target differs from its manifest")
                    if firmware:
                        from firmware_signing import keys, key_id, verify_image
                        public, _ = keys(firmware, release=firmware["profile"] != "test")
                        key = next((key for key in public if key_id(key) == manifest["secure_boot_key_id"]), None)
                        if key is None:
                            raise ValueError("public image uses an unconfigured firmware signing key")
                        verify_image(image, key)

def publish(command, track, files, previous_timestamp, bootstrap, targets, checkpoint=lambda: None):
    origins = ORIGINS[track]
    for path, data in sorted(files.items()):
        if path == "metadata/timestamp.json":
            continue
        upload(command, track, path, data)
        for origin in origins:
            fetch(origin, path, data)
    # Repeatable after a lost receipt: the adapter accepts byte-identical data.
    upload(command, track, "metadata/timestamp.json", files["metadata/timestamp.json"], previous_timestamp)
    checkpoint()
    for origin in origins:
        fetch(origin, "metadata/timestamp.json", files["metadata/timestamp.json"])
    verify_public(origins, bootstrap, targets)
