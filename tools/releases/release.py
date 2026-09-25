#!/usr/bin/env python3
"""Build, sign and publish a release through explicit external adapters."""
from __future__ import annotations
import argparse
import copy
from datetime import datetime, timezone
import fcntl
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.error

from tuf.api.metadata import Metadata, Targets
from tuf.api.serialization.json import JSONSerializer
from repository import Repository, Signers, THRESHOLDS, CHANNELS, RELEASE_PROFILES, atomic, immutable, encoded, digest, init_test_keys
from firmware_signing import board_family, sign_image, verify_image, key_id, keys as firmware_keys
from publisher import ORIGINS, fetch, publish, verify_public

SOURCE = Path(__file__).resolve().parents[2]
# Release numbers are shared, so a number names one source commit and one track.
TAGS = {"trusted": "firmware-{}", "open": "firmware-open-{}"}
APPROVALS = {
    "trusted": "the trusted track is not approved; complete and retain the commissioning and soak checklist first",
    "open": "the open track is not approved; back up its signing keys and confirm both open origins serve before releasing",
}

def git(*args, cwd=SOURCE):
    return subprocess.check_output(["git", *args], cwd=cwd, text=True).strip()

def command_available(command):
    return isinstance(command, list) and command and all(isinstance(x, str) and x for x in command) and shutil.which(command[0]) is not None

def image_identity(image):
    if len(image)<304 or image[32:36]!=b"\x32\x54\xcd\xab" or image[80:112].split(b"\0",1)[0]!=b"navfeeder-esp":
        raise ValueError("release image has the wrong application descriptor")
    version=image[48:80].split(b"\0",1)[0].decode("ascii")
    match=re.fullmatch(r"[^+]+\+([1-9][0-9]*)\.([0-9a-f]{7})(-dirty)?",version)
    if not match: raise ValueError("release image lacks its compiled build/revision identity")
    return int(match[1]),match[2],bool(match[3])

def configuration(path):
    if not path:
        raise ValueError("No release configuration. Start with make release-test-setup; see esp32/docs/UPDATE-OPERATIONS.md before releasing to a track.")
    path = Path(path).expanduser().resolve(strict=True)
    if path.is_relative_to(SOURCE):
        raise ValueError("release configuration and private key locations must stay outside the checkout")
    config = json.loads(path.read_bytes())
    track = config.get("track")
    if track not in RELEASE_PROFILES:
        raise ValueError("release configuration must name its track: trusted or open")
    # One configuration serves one track. Approval for the other is meaningless
    # here and usually means a copied file that still points at the wrong keys.
    if any(config.get(f"{other}_approved") is not None for other in RELEASE_PROFILES if other != track):
        raise ValueError(f"a {track} release configuration cannot carry another track's approval")
    if config.get(f"{track}_approved") is not True:
        raise ValueError(APPROVALS[track])
    for name in ("state_dir", "signers", "root"):
        value = Path(config[name]).expanduser()
        if not value.is_absolute() or value.resolve().is_relative_to(SOURCE):
            raise ValueError(f"{name} must be an explicit external absolute path")
        config[name] = str(value.resolve())
    signers = Signers(Path(config["signers"]), profile=track)
    firmware_keys(signers.config, True)
    config["origins"] = ORIGINS[track]
    for name in ("build_command", "publish_command"):
        if not command_available(config.get(name)):
            raise ValueError(f"{name} adapter is unavailable")
    if set(config.get("remotes", [])) != {"origin", "github"} or config.get("branch") != "main":
        raise ValueError("releases require main and both origin/github software remotes")
    bootstrap = Path(config["root"]).read_bytes()
    root = Metadata.from_bytes(bootstrap)
    if root.signed.unrecognized_fields.get("x_navlisten_profile") != track:
        raise ValueError(f"the {track} track refuses a bootstrap root marked for another profile or unmarked")
    root.verify_delegate("root", root)
    return config, signers, bootstrap

def signer_entry(path, *, firmware):
    """The public half of one signers.json entry. No private material is read."""
    from cryptography.hazmat.primitives import serialization
    key = serialization.load_pem_public_key(Path(path).expanduser().read_bytes())
    if firmware:
        return key_id(key)
    from cryptography.hazmat.primitives.asymmetric import ec
    from securesystemslib.signer import SSlibKey
    if not isinstance(key, ec.EllipticCurvePublicKey) or not isinstance(key.curve, ec.SECP256R1):
        raise ValueError("metadata keys must use ECDSA P-256")
    public = SSlibKey.from_crypto(key)
    return {"keyid": public.keyid, **public.to_dict()}

def clean(config):
    if git("branch", "--show-current") != config["branch"] or git("status", "--porcelain", "--untracked-files=no"):
        raise ValueError("release requires the clean main branch; finish or isolate other work first")
    for remote in config["remotes"]:
        git("remote", "get-url", "--push", remote)
    pin = json.loads((SOURCE / "tools/releases/toolchain.json").read_bytes())
    idf = Path(os.environ.get("IDF_PATH", "/nonexistent"))
    if not idf.is_dir() or git("rev-parse", "HEAD", cwd=idf) != pin["esp_idf_revision"]:
        raise ValueError("activate the pinned ESP-IDF v5.5.4 environment before releasing")

def save_transaction(directory, transaction, signers):
    role = "releases" if directory.name.isdecimal() else "timestamp"
    envelope = Metadata(Targets(unrecognized_fields={"transaction": transaction}))
    # Use the appropriate signature adapters, retaining a signed compact record
    # of the immutable inventory digest rather than copying artifact bytes.
    atomic(directory / "transaction.json", signers.sign(role, envelope), private=True)

def load_transaction(directory, signers):
    role = "releases" if directory.name.isdecimal() else "timestamp"
    envelope = Metadata.from_file(str(directory / "transaction.json"))
    valid = set()
    for key in signers.keys[role]:
        signature = envelope.signatures.get(key.keyid)
        if signature:
            key.verify_signature(signature, envelope.signed_bytes); valid.add(key.keyid)
    if len(valid) < THRESHOLDS[role]:
        raise ValueError("transaction signature threshold failed")
    return envelope.signed.unrecognized_fields["transaction"]

def inventory(directory):
    return {str(p.relative_to(directory)): digest(p.read_bytes()) for p in sorted(directory.rglob("*")) if p.is_file()}

def verify_inventory(directory, expected):
    for name, sha in expected.items():
        path = Path(name)
        if path.is_absolute() or ".." in path.parts or digest((directory / name).read_bytes()) != sha:
            raise ValueError("transaction output differs; never replace an existing release artifact")

def public_timestamp(origins):
    values = []
    for origin in origins:
        try:
            values.append(digest(fetch(origin, "metadata/timestamp.json")))
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
            values.append(None)
    if values[0] != values[1]:
        raise ValueError("the primary and secondary firmware origins disagree")
    return values[0]

def validate_repository(repository, bootstrap):
    checked = Repository(repository.directory, repository.signers, now=repository.now)
    checked.load(bootstrap)
    for role in ("releases", *CHANNELS):
        for path, target in checked.metadata[role].signed.targets.items():
            target.verify_length_and_hashes(checked.target_bytes(role, path))
    return checked

def release_build(directory, tx, config, signers):
    if tx["phase"] == "reserved":
        outputs = []
        for index in (1, 2):
            clone = directory / f"source-{index}"
            receipt = directory / f"build-{index}.json"
            if not receipt.exists():
                if not clone.exists():
                    subprocess.run(["git", "clone", "--quiet", "--shared", "--no-checkout", str(SOURCE), str(clone)], check=True)
                    subprocess.run(["git", "checkout", "--quiet", "--detach", tx["revision"]], cwd=clone, check=True)
                if git("rev-parse", "HEAD", cwd=clone) != tx["revision"]:
                    raise ValueError("isolated build checkout changed")
                if index == 1:
                    subprocess.run(["make", "-C", "go", "check"], cwd=clone, check=True)
                request = {"source": str(clone), "build": str(directory / f"build-{index}"), "revision": tx["revision"], "root": config["root"], "profile": config["track"]}
                result = subprocess.run(config["build_command"], cwd=clone, input=encoded(request), stdout=subprocess.PIPE, check=True)
                paths = json.loads(result.stdout)
                values = {name: {"path": path, "sha256": digest(Path(path).read_bytes())} for name, path in paths.items()}
                atomic(receipt, encoded(values), private=True)
            values = json.loads(receipt.read_bytes())
            for value in values.values():
                if digest(Path(value["path"]).read_bytes()) != value["sha256"]:
                    raise ValueError("a completed build output changed")
            outputs.append(values)
        if any(outputs[0][name]["sha256"] != outputs[1][name]["sha256"] for name in ("elf", "image")):
            raise ValueError("independent release ELFs and padded images must match byte-for-byte")
        tx["build"] = outputs[0]; tx["phase"] = "built"; save_transaction(directory, tx, signers)
    if tx["phase"] == "built":
        for output in tx["build"].values():
            if digest(Path(output["path"]).read_bytes()) != output["sha256"]:
                raise ValueError("recorded build changed")
        unsigned = Path(tx["build"]["image"]["path"]).read_bytes()
        if image_identity(unsigned)!=(tx["sequence"],tx["revision"][:7],False):
            raise ValueError("compiled image identity differs from the reserved release commit")
        if (directory / "signed-image.bin").is_file():
            image = (directory / "signed-image.bin").read_bytes()
            public, active = firmware_keys(signers.config, True)
            verify_image(image, public[active]); key = key_id(public[active])
            if image[:-4096] != unsigned + b"\xff" * (-len(unsigned) % 4096):
                raise ValueError("interrupted signing output belongs to a different build")
        else:
            image, key = sign_image(unsigned, signers.config, True)
            immutable(directory / "signed-image.bin", image)
        tx["image_sha256"] = digest(image); tx["firmware_key_id"] = key; tx["phase"] = "signed"
        save_transaction(directory, tx, signers)

def finish(directory, tx, config, signers, bootstrap):
    if tx["phase"] == "preparing":
        prepare_metadata(directory,tx,config,signers,bootstrap)
    if tx["phase"] in ("reserved", "built"):
        release_build(directory, tx, config, signers)
    candidate = directory / "candidate"
    if tx["phase"] == "signed":
        if digest((directory / "signed-image.bin").read_bytes()) != tx["image_sha256"]:
            raise ValueError("signed image changed")
        if not candidate.exists():
            shutil.copytree(Path(config["state_dir"]) / "repository", candidate)
        bundle_path = directory / "candidate-bundle.json"
        if not bundle_path.exists():
            repository = Repository(candidate, signers); repository.load(bootstrap)
            notes = (directory / "notes.md").read_bytes()
            if digest(notes) != tx["notes_sha256"]:
                raise ValueError("release notes changed after number reservation")
            licenses = json.loads(Path(tx["build"]["licenses"]["path"]).read_bytes())
            signed = (directory / "signed-image.bin").read_bytes()
            repository.add_release(sequence=tx["sequence"], version=tx["version"], revision=tx["revision"], image=signed,
                board_family=board_family(signed), boot_key_id=tx["firmware_key_id"], provenance=Path(tx["build"]["provenance"]["path"]).read_bytes(), licenses=encoded(licenses),
                notes=notes, layout=3, hardware_min=config.get("hardware_min", 1), hardware_max=config.get("hardware_max", 1))
            import base64
            immutable(bundle_path, encoded({path: base64.b64encode(data).decode() for path, data in repository.files.items()}))
        import base64
        bundle = {path: base64.b64decode(value, validate=True) for path, value in json.loads(bundle_path.read_bytes()).items()}
        repository = Repository(candidate, signers); repository.files = bundle; repository.publish_local()
        checked = validate_repository(repository, bootstrap)
        manifest = json.loads(checked.target_bytes("releases", f"releases/{tx['sequence']}.json"))
        if manifest["sha256"] != tx["image_sha256"] or manifest["source_revision"] != tx["revision"] or manifest["build_number"] != tx["sequence"]:
            raise ValueError("interrupted candidate differs from its signed release transaction")
        freeze_candidate(directory, tx, signers)
    if tx["phase"] in ("prepared", "source-pushed", "published"):
        index_bytes = (directory / "inventory.json").read_bytes()
        if digest(index_bytes) != tx["inventory_sha256"]:
            raise ValueError("transaction inventory signature differs")
        index = json.loads(index_bytes); verify_inventory(candidate, index)
        if tx["phase"] == "prepared" and "revision" in tx:
            tag = TAGS[config["track"]].format(tx["sequence"])
            try:
                revision = git("rev-parse", f"{tag}^{{commit}}")
            except subprocess.CalledProcessError:
                subprocess.run(["git", "tag", "-a", tag, tx["revision"], "-m", f"Firmware release {tx['sequence']} on the {config['track']} track; initial channel Lab"], cwd=SOURCE, check=True)
                revision = tx["revision"]
            if revision != tx["revision"]:
                raise ValueError("release tag already names a different source commit")
            for remote in config["remotes"]:
                subprocess.run(["git", "push", remote, f"{tx['revision']}:refs/heads/{config['branch']}", f"refs/tags/{tag}"], cwd=SOURCE, check=True)
            tx["phase"] = "source-pushed"; save_transaction(directory, tx, signers)
        files = {name: (candidate / name).read_bytes() for name in index}
        repository = Repository(candidate, signers); repository.load(bootstrap)
        targets = list(repository.metadata["releases"].signed.targets)
        def committed():
            tx["phase"] = "published"; save_transaction(directory, tx, signers)
        publish(config["publish_command"], config["track"], files, tx["previous_timestamp"], bootstrap, targets, committed)
        # Bring the local cache to the verified public commit without changing
        # or deleting historical immutable objects.
        for name, data in files.items():
            dest = Path(config["state_dir"]) / "repository" / name
            atomic(dest, data) if name == "metadata/timestamp.json" else immutable(dest, data)
        tx["phase"] = "complete"; save_transaction(directory, tx, signers)
    print(f"Transaction {directory.name}: {tx['phase']}")
    if "sequence" in tx:
        print(f"Release {tx['sequence']} is selected for 100% of Lab on the {config['track']} track. Inspect device status before promotion.")
        print(f"make release-promote RELEASE={tx['sequence']} CHANNEL=canary PERCENT=1")
        print(f"make release-withdraw RELEASE={tx['sequence']} CHANNEL=lab")

def freeze_candidate(directory, tx, signers):
    data = encoded(inventory(directory / "candidate")); immutable(directory / "inventory.json", data)
    tx["inventory_sha256"] = digest(data); tx["phase"] = "prepared"; save_transaction(directory, tx, signers)

def prepare_metadata(directory,tx,config,signers,bootstrap):
    import base64
    candidate=directory/"candidate"
    base=Path(config["state_dir"])/"repository"
    # Repeating an interrupted copy is safe: immutable objects must match.
    for path in sorted(base.rglob("*")):
        if path.is_file() and path.relative_to(base).as_posix()!="metadata/timestamp.json":
            immutable(candidate/path.relative_to(base),path.read_bytes())
    bundle_path=directory/"candidate-bundle.json"
    if not bundle_path.exists():
        atomic(candidate/"metadata/timestamp.json",(base/"metadata/timestamp.json").read_bytes())
        repository=Repository(candidate,signers);repository.load(bootstrap)
        if tx["action"]=="refresh-online":
            for channel in CHANNELS:repository.metadata_for(channel,copy.deepcopy(repository.metadata[channel].signed))
        else:
            path=f"releases/{tx['release']}.json"
            if path not in repository.metadata["releases"].signed.targets:raise ValueError("release is not authorized by the offline release role")
            old=repository.channel_value(tx["channel"])
            if tx["action"]=="withdraw":
                repository.set_channel(tx["channel"],old["release_sequence"],old["release_manifest"],percentage=old["percentage"],withdrawn=sorted(set(old["withdrawn"]+[tx["release"]])))
            else:
                if tx["release"] in old["withdrawn"]:raise ValueError("withdrawn release cannot be promoted; publish a new release")
                repository.set_channel(tx["channel"],tx["release"],path,percentage=tx["percent"])
        repository.online()
        immutable(bundle_path,encoded({path:base64.b64encode(data).decode() for path,data in repository.files.items()}))
    repository=Repository(candidate,signers)
    repository.files={path:base64.b64decode(value,validate=True) for path,value in json.loads(bundle_path.read_bytes()).items()}
    repository.publish_local();validate_repository(repository,bootstrap);freeze_candidate(directory,tx,signers)

def license_inventory(source):
    paths = [source/line for line in git("ls-files",cwd=source).splitlines() if Path(line).name.startswith(("LICENSE","COPYING"))]
    return [{"path": str(p.relative_to(source)), "sha256": digest(p.read_bytes()), "text": p.read_text()} for p in paths]

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default=os.environ.get("NAVLISTEN_RELEASE_CONFIG"))
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("init-test", "init-trusted", "init-open"):
        p = sub.add_parser(name); p.add_argument("--keys", required=True, type=Path); p.add_argument("--state", required=True, type=Path)
    p = sub.add_parser("test-release"); p.add_argument("--keys", required=True, type=Path); p.add_argument("--state", required=True, type=Path)
    p.add_argument("--image", required=True, type=Path); p.add_argument("--release", type=int, required=True)
    p = sub.add_parser("release"); p.add_argument("--notes", required=True, type=Path)
    sub.add_parser("dry-run")
    p = sub.add_parser("resume"); p.add_argument("--release", required=True)
    for name in ("promote", "withdraw", "check"):
        p = sub.add_parser(name); p.add_argument("--release", required=True, type=int)
        if name != "check": p.add_argument("--channel", choices=CHANNELS, required=True)
        if name == "promote": p.add_argument("--percent", type=int, required=True)
    sub.add_parser("refresh-online")
    p = sub.add_parser("signer-entry", help="print the public half of a signers.json entry for one public key")
    p.add_argument("--public-key", required=True, type=Path); p.add_argument("--firmware", action="store_true")
    args = parser.parse_args()
    if args.command=="promote" and not 0<=args.percent<=100:raise ValueError("rollout percentage must be between 0 and 100")
    if args.command == "signer-entry":
        print(json.dumps(signer_entry(args.public_key, firmware=args.firmware), indent=2)); return
    if args.command.startswith("init-"):
        state = args.state.expanduser().resolve()
        if state.is_relative_to(SOURCE): raise ValueError("release state must live outside the checkout")
        if state.exists(): raise ValueError("release state already exists; initialization never overwrites it")
        # Reject an unusable signer configuration before leaving any state behind.
        if args.command != "init-test":
            firmware_keys(Signers(args.keys, profile=args.command.removeprefix("init-")).config, True)
        state.mkdir(parents=True, mode=0o700, exist_ok=False)
        path = init_test_keys(args.keys) if args.command == "init-test" else args.keys
        signers = Signers(path, profile=args.command.removeprefix("init-"))
        repository = Repository(state / "repository", signers); repository.bootstrap(); repository.publish_local()
        atomic(state / "trust-root.json", repository.files["metadata/1.root.json"])
        print(f"Created {'TEST-ONLY' if signers.test_only else signers.profile} repository and public trust root in {state}")
        print(f"Private signer configuration: {path}. Back up the entire key directory securely; never copy it into Git or onto the origin.")
        return
    if args.command == "test-release":
        signers = Signers(args.keys.expanduser(), profile="test")
        state = args.state.expanduser().resolve(); root = (state / "trust-root.json").read_bytes()
        repository = Repository(state / "repository", signers); repository.load(root)
        unsigned=args.image.read_bytes()
        if image_identity(unsigned)[0]!=args.release:
            raise ValueError("test release number must match the image's compiled BUILD_NUMBER")
        image, key = sign_image(unsigned, signers.config, False)
        repository.add_release(sequence=args.release, version="0.0.0-test", revision=git("rev-parse", "HEAD"), image=image,
            board_family=board_family(image), boot_key_id=key,
            provenance=encoded({"profile": "test"}), licenses=encoded(license_inventory(SOURCE)), notes=b"TEST ONLY; never publish to a release track.\n", layout=3)
        repository.publish_local(); validate_repository(repository, root)
        print(f"Signed TEST-ONLY release {args.release} locally. No source push, remote publication or chip lock was performed.")
        return
    config, signers, bootstrap = configuration(args.config)
    state = Path(config["state_dir"]); state.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (state / "release.lock").open("a+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.command == "resume":
            if not args.release or any(c not in "0123456789metadata-" for c in args.release): raise ValueError("invalid transaction name")
            directory = state / "transactions" / args.release; tx = load_transaction(directory, signers)
            if tx["phase"] == "reserving":
                if git("rev-parse","HEAD")==tx["parent"]:
                    changed=git("diff","--name-only","HEAD").splitlines()
                    if changed not in ([],["BUILD_NUMBER"]):raise ValueError("reservation source changed; finish other work before resuming")
                    if int((SOURCE/"BUILD_NUMBER").read_text()) not in (tx["sequence"]-1,tx["sequence"]):raise ValueError("reserved build number changed")
                    atomic(SOURCE/"BUILD_NUMBER",f"{tx['sequence']}\n".encode())
                    subprocess.run(["git","add","--","BUILD_NUMBER"],cwd=SOURCE,check=True)
                    subprocess.run(["git","commit","-m",f"Reserve firmware release {tx['sequence']}","--","BUILD_NUMBER"],cwd=SOURCE,check=True)
                if int((SOURCE / "BUILD_NUMBER").read_text()) != tx["sequence"] or git("rev-parse","HEAD^")!=tx["parent"] or git("log", "-1", "--format=%s") != f"Reserve firmware release {tx['sequence']}":
                    raise ValueError("reservation did not finish; inspect the signed transaction and source commit before continuing")
                tx["revision"] = git("rev-parse", "HEAD"); tx["phase"] = "reserved"; save_transaction(directory, tx, signers)
            finish(directory, tx, config, signers, bootstrap); return
        if args.command == "check":
            verify_public(config["origins"], bootstrap, [f"releases/{args.release}.json"], signers.config); print("Both public origins passed TUF, image signature, provenance, license and release-note verification."); return
        for path in (state / "transactions").glob("*/transaction.json"):
            if load_transaction(path.parent, signers)["phase"] != "complete":
                raise ValueError(f"unfinished transaction {path.parent.name}; use release-resume")
        clean(config)
        if args.command == "dry-run":
            print(f"The {config['track']} track preflight passed. No number reserved, signer called, build run, source pushed or file published."); return
        previous = public_timestamp(config["origins"])
        cached = state / "repository/metadata/timestamp.json"
        if previous is not None and digest(cached.read_bytes()) != previous:
            raise ValueError("local repository cache differs from public timestamp; reconcile it before reserving a version")
        if args.command == "release":
            sequence = int((SOURCE / "BUILD_NUMBER").read_text()) + 1
            if not 0 < sequence < 2**64: raise ValueError("release number exhausted")
            directory = state / "transactions" / str(sequence); directory.mkdir(parents=True, exist_ok=False)
            notes = args.notes.read_bytes(); immutable(directory / "notes.md", notes)
            tx = {"phase": "reserving", "sequence": sequence, "version": (SOURCE / "VERSION").read_text().strip(),
                "parent": git("rev-parse", "HEAD"), "notes_sha256": digest(notes), "previous_timestamp": previous}
            save_transaction(directory, tx, signers)
            atomic(SOURCE / "BUILD_NUMBER", f"{sequence}\n".encode())
            subprocess.run(["git", "add", "--", "BUILD_NUMBER"], cwd=SOURCE, check=True)
            subprocess.run(["git", "commit", "-m", f"Reserve firmware release {sequence}", "--", "BUILD_NUMBER"], cwd=SOURCE, check=True)
            tx["revision"] = git("rev-parse", "HEAD"); tx["phase"] = "reserved"; save_transaction(directory, tx, signers)
        else:
            base = Repository(state / "repository", signers); base.load(bootstrap)
            if args.command in ("promote","withdraw"):
                if f"releases/{args.release}.json" not in base.metadata["releases"].signed.targets:raise ValueError("release is not authorized by the offline release role")
                old=base.channel_value(args.channel)
                if args.command=="promote" and args.release in old["withdrawn"]:raise ValueError("withdrawn release cannot be promoted; publish a new release")
                if args.command=="withdraw" and len(set(old["withdrawn"]+[args.release]))>32:raise ValueError("withdrawal list exceeds the device profile")
            directory = state / "transactions" / f"metadata-{base.versions['timestamp'] + 1}"; directory.mkdir(parents=True, exist_ok=False)
            tx = {"phase": "preparing", "previous_timestamp": previous,"action":args.command,
                "release":getattr(args,"release",None),"channel":getattr(args,"channel",None),"percent":getattr(args,"percent",None)}
            save_transaction(directory,tx,signers)
        finish(directory, tx, config, signers, bootstrap)

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        raise SystemExit(str(error)) from None
