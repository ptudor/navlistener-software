#!/usr/bin/env python3
"""Build adapter for an isolated release checkout; activate ESP-IDF first."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

def run(command, cwd):
    subprocess.run(command, cwd=cwd, check=True, stdout=sys.stderr)

def license_inventory(source, idf, project):
    notices = {}
    # IDF includes an empty path for its synthetic component. Path("") would
    # scan the publisher's working directory instead of an isolated build input.
    directories = [source, idf, *(Path(p) for p in project["build_component_paths"] if p)]
    for directory in directories:
        if not directory.is_absolute():
            raise ValueError("license source must be an absolute build input")
        paths = directory.iterdir() if directory in (source, idf) else directory.rglob("*")
        for path in paths:
            if not path.is_file() or not path.name.upper().startswith(("LICENSE", "COPYING", "NOTICE")):
                continue
            resolved = path.resolve()
            if resolved.is_relative_to(source):
                name = resolved.relative_to(source).as_posix()
            elif resolved.is_relative_to(idf):
                name = "esp-idf/" + resolved.relative_to(idf).as_posix()
            else:
                raise ValueError("license source is outside the pinned build inputs")
            data = path.read_bytes()
            notices[name] = {"path": name, "sha256": hashlib.sha256(data).hexdigest(), "text": data.decode("utf-8")}
    return [notices[k] for k in sorted(notices)]

def main():
    request = json.load(sys.stdin)
    source, build = Path(request["source"]).resolve(strict=True), Path(request["build"]).resolve()
    idf = Path(os.environ["IDF_PATH"]).resolve(strict=True)
    python = Path(os.environ["IDF_PYTHON_ENV_PATH"]) / "bin/python"
    pin = json.loads((source / "tools/releases/toolchain.json").read_bytes())
    revision = subprocess.check_output(["git", "-C", str(idf), "rev-parse", "HEAD"], text=True).strip()
    if revision != pin["esp_idf_revision"] or subprocess.check_output(["git", "-C", str(idf), "status", "--porcelain", "--untracked-files=no"]):
        raise ValueError("activate the pinned, clean ESP-IDF checkout before releasing")
    if os.environ.get("ESP_HARDWARE_DISCOVERY_PATH"):
        raise ValueError("production builds refuse local component overrides")
    run([str(python), str(idf / "tools/idf.py"), "-B", str(build), "-D", f"SDKCONFIG={build}/sdkconfig",
        "-D", "SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3;sdkconfig.defaults.production",
        "-D", "IDF_TARGET=esp32s3", "-D", f"NVF_TUF_ROOT_FILE={request['root']}",
        "-D", f"NVF_RELEASE_REVISION={request['revision']}", "build"], source / "esp32")
    run([str(python), "tools/production_profile.py", str(build / "sdkconfig")], source / "esp32")
    run([str(python), "tools/build_provenance.py", "--build-dir", str(build)], source / "esp32")
    provenance = json.loads((build / "firmware-provenance.json").read_bytes())
    if not provenance["dependencies_lock_matches_pin"] or provenance["hardware_discovery"]["local_override"]:
        raise ValueError("production dependency provenance is not pinned")
    provenance.pop("project_dirty", None)  # IDF regenerates its target-specific resolution.
    provenance["source_revision"] = request["revision"]
    provenance["elf_sha256"] = hashlib.sha256((build / "navfeeder-esp.elf").read_bytes()).hexdigest()
    provenance["unsigned_image_sha256"] = hashlib.sha256((build / "navfeeder-esp.bin").read_bytes()).hexdigest()
    (build / "release-provenance.json").write_text(json.dumps(provenance, sort_keys=True, indent=2) + "\n")
    project=json.loads((build/"project_description.json").read_bytes())
    (build/"release-licenses.json").write_text(json.dumps(license_inventory(source, idf, project),sort_keys=True)+"\n")
    print(json.dumps({"image": str(build / "navfeeder-esp.bin"), "elf": str(build / "navfeeder-esp.elf"),
        "provenance": str(build / "release-provenance.json"),"licenses":str(build/"release-licenses.json")}))

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        raise SystemExit(str(error)) from None
