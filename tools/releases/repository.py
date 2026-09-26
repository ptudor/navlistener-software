"""TUF repository construction using the pinned reference implementation.

Private signing material is supplied by adapters. Only init_test_keys creates
keys, in an explicit private directory with an unambiguous test-only marker.

Every signer configuration, root and release manifest names one trust profile.
Trusted and open are release tracks with separate roots and real keys; test is
the throwaway bench profile. None can authorize firmware for another.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path
import subprocess
import tempfile

from cryptography.hazmat.primitives import serialization
from securesystemslib.signer import CryptoSigner, SSlibKey, Signature
from tuf.api.metadata import (
    Metadata, Root, Role, Targets, Snapshot, Timestamp, Delegations,
    DelegatedRole, TargetFile, MetaFile,
)
from tuf.api.serialization.json import CanonicalJSONSerializer, JSONSerializer

# Byte 9 of an image's NVFOTA1 metadata names the board it was built for
# (esp32/components/ota/include/nvf_board.h); a release names the same family.
# NVFOTA1 byte 9. The universal image (4) drives only what each board's manifest lists,
# so its releases fit every board; the others are single-board test images.
BOARD_FAMILIES = {1: "gnss-color-neo", 2: "gnss-color-zed-x20", 3: "gnss-color-max", 4: "gnss-color"}

CHANNELS = ("stable", "canary", "lab")
PROFILES = ("trusted", "open", "test")
RELEASE_PROFILES = ("trusted", "open")
COUNTS = {"root": 3, "targets": 3, "releases": 3, **{c: 1 for c in CHANNELS}, "snapshot": 1, "timestamp": 1}
THRESHOLDS = {r: 2 if n == 3 else 1 for r, n in COUNTS.items()}
LIMITS = {"root": 8192, "timestamp": 4096, "snapshot": 8192, **{r: 12288 for r in ("targets", "releases", *CHANNELS)}}
LIFETIMES = {"root": 730, "targets": 365, "releases": 365, "timestamp": 14, "snapshot": 45, **{c: 45 for c in CHANNELS}}


def encoded(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def object_path(root, path):
    """Constrain repository paths before creating any directory or opening a file."""
    if not isinstance(path, str) or len(path) > 280 or not re.fullmatch(r"(?:metadata|targets)/[A-Za-z0-9._/-]+", path) or ".." in path or "//" in path or path.endswith("/"):
        raise ValueError("invalid repository object path")
    root = root.resolve()
    target = root / path
    for part in [target, *target.parents]:
        if part == root:
            break
        if part.is_symlink():
            raise ValueError("repository object path contains a symlink")
    return target


def atomic(path: Path, data: bytes, *, private=False):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700 if private else 0o755)
    descriptor, name = tempfile.mkstemp(prefix=".write-", dir=path.parent)
    temporary = Path(name)
    os.fchmod(descriptor, 0o600 if private else 0o644)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


def immutable(path: Path, data: bytes):
    if path.exists():
        if path.read_bytes() != data:
            raise ValueError(f"immutable object differs: {path.name}")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    # Write a private temporary inode, then publish with link() without clobbering.
    import tempfile
    descriptor, name = tempfile.mkstemp(prefix=".publish-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, 0o644)
        try:
            os.link(temporary, path)
        except FileExistsError:
            if path.read_bytes() != data:
                raise ValueError(f"immutable object differs: {path.name}") from None
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


def init_test_keys(directory: Path):
    directory = directory.expanduser().resolve()
    checkout = Path(__file__).resolve().parents[2]
    if directory.is_relative_to(checkout):
        raise ValueError("private test keys must be stored outside the source checkout")
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    config = {"profile": "test", "roles": {}}
    for role, count in COUNTS.items():
        config["roles"][role] = []
        for index in range(count):
            signer = CryptoSigner.generate_ecdsa()
            private = directory / f"TEST-ONLY-{role}-{index}.key"
            atomic(private, signer.private_bytes, private=True)
            config["roles"][role].append({"public": {"keyid": signer.public_key.keyid,
                **signer.public_key.to_dict()}, "test_key": str(private)})
    from firmware_signing import init_test_firmware_keys
    init_test_firmware_keys(directory, config)
    atomic(directory / "TEST-KEYS-DO-NOT-USE-FOR-PRODUCTION", b"These keys authorize development fixtures only.\n", private=True)
    atomic(directory / "signers.json", encoded(config), private=True)
    return directory / "signers.json"


class Signers:
    def __init__(self, path: Path, *, profile: str):
        if profile not in PROFILES:
            raise ValueError("unknown trust profile")
        self.path = path.expanduser().resolve()
        self.config = json.loads(self.path.read_bytes())
        self.profile = profile
        self.test_only = profile == "test"
        # Each track keeps its own signer configuration, so a root is never
        # bootstrapped or extended with another track's keys by mistake.
        if self.config.get("profile") != profile:
            raise ValueError(f"{profile} releases require a separate signer configuration whose profile is {profile}")
        self.keys = {}
        for role, count in COUNTS.items():
            specs = self.config["roles"][role]
            if len(specs) != count:
                raise ValueError(f"{role} requires {count} independent public keys")
            self.keys[role] = []
            for spec in specs:
                public = dict(spec["public"])
                key = SSlibKey.from_dict(public.pop("keyid"), public)
                if key.keytype != "ecdsa" or key.scheme != "ecdsa-sha2-nistp256":
                    raise ValueError("metadata keys must use ECDSA P-256")
                if not self.test_only and ("test_key" in spec or (spec.get("command") is not None and (not isinstance(spec["command"], list) or not spec["command"]))):
                    raise ValueError("release signing requires an explicit adapter; test key paths are forbidden")
                from cryptography.hazmat.primitives.asymmetric import ec
                public_key = serialization.load_pem_public_key(key.keyval["public"].encode())
                if not isinstance(public_key, ec.EllipticCurvePublicKey) or not isinstance(public_key.curve, ec.SECP256R1) or SSlibKey.from_crypto(public_key).keyid != key.keyid:
                    raise ValueError("metadata public key identity or curve is invalid")
                self.keys[role].append(key)
        ids = [key.keyid for keys in self.keys.values() for key in keys]
        if len(ids) != len(set(ids)):
            raise ValueError("each key must be independent and assigned to exactly one role")

    def sign(self, role, metadata):
        payload = metadata.signed_bytes
        metadata.signatures.clear()
        for key, spec in zip(self.keys[role], self.config["roles"][role]):
            if self.test_only:
                path = Path(spec["test_key"])
                if not path.is_absolute() or path.stat().st_mode & 0o077:
                    raise ValueError("test key must have a private absolute path and mode 0600")
                private = serialization.load_pem_private_key(path.read_bytes(), password=None)
                signature = CryptoSigner(private, key).sign(payload)
            else:
                if spec.get("command") is None:
                    continue  # the third offline key need not be online to meet 2-of-3
                # Adapter reads canonical bytes on stdin and returns only a TUF
                # signature object on stdout. No shell, key material or log capture.
                result = subprocess.run(spec["command"], input=payload, stdout=subprocess.PIPE, check=True, timeout=300)
                signature = Signature.from_dict(json.loads(result.stdout))
            if signature.keyid != key.keyid:
                raise ValueError("signer returned the wrong key identity")
            key.verify_signature(signature, payload)
            metadata.signatures[key.keyid] = signature
        if len(metadata.signatures) < THRESHOLDS[role]:
            raise ValueError(f"{role} signing threshold is unavailable; connect the required adapters")
        data = metadata.to_bytes(JSONSerializer(compact=True))
        if len(data) > LIMITS[role]:
            raise ValueError(f"{role} metadata exceeds the device's bounded profile")
        return data


class Repository:
    def __init__(self, directory: Path, signers: Signers, *, now=None):
        self.directory = directory.resolve()
        self.signers = signers
        self.now = (now or datetime.now(timezone.utc)).replace(microsecond=0)
        self.metadata = {}
        self.versions = {}
        self.files = {}

    def metadata_for(self, role, body):
        body.version = self.versions.get(role, 0) + 1
        body.spec_version = "1.0.36"
        body.expires = self.now + timedelta(days=LIFETIMES[role])
        metadata = Metadata(body)
        data = self.signers.sign(role, metadata)
        self.metadata[role] = metadata
        self.versions[role] = body.version
        path = f"metadata/{body.version}.{role}.json" if role != "timestamp" else "metadata/timestamp.json"
        self.files[path] = data
        return data

    def bootstrap(self):
        root = Root(roles={r: Role([], THRESHOLDS[r]) for r in ("root", "targets", "snapshot", "timestamp")},
                    unrecognized_fields={"x_navlisten_profile": self.signers.profile})
        for role in root.roles:
            for key in self.signers.keys[role]:
                root.add_key(key, role)
        self.metadata_for("root", root)
        keys = {}
        roles = {}
        for role in ("releases", *CHANNELS):
            keys.update({key.keyid: key for key in self.signers.keys[role]})
            paths = ["releases/*", "artifacts/*", "notes/*"] if role == "releases" else [f"channels/{role}.json"]
            roles[role] = DelegatedRole(role, [key.keyid for key in self.signers.keys[role]], THRESHOLDS[role], True, paths)
        self.metadata_for("targets", Targets(delegations=Delegations(keys, roles)))
        self.metadata_for("releases", Targets())
        for channel in CHANNELS:
            self.set_channel(channel, 0, "", generation=1)
        self.online()

    def add_target(self, role, path, data):
        if path.startswith("/") or ".." in path or "\\" in path or len(path.encode()) > 192:
            raise ValueError("invalid target path")
        target = TargetFile.from_data(path, data, ["sha256"])
        name = Path(path)
        published = name.with_name(target.hashes["sha256"] + "." + name.name)
        self.files["targets/" + published.as_posix()] = data
        role.targets[path] = target
        if len(role.targets) > 32:
            raise ValueError("targets role exceeds 32 entries; archive retired releases before adding another")
        return target

    def set_channel(self, channel, sequence, manifest, *, percentage=100, generation=None, withdrawn=None, salt=None, advisory="", priority="normal"):
        if channel not in CHANNELS or not 0 <= percentage <= 100:
            raise ValueError("invalid channel or rollout percentage")
        old = self.channel_value(channel) if channel in self.metadata else None
        choice = {"schema": 1, "generation": generation or ((old["generation"] if old else 0) + 1),
                  "release_sequence": sequence, "release_manifest": manifest, "priority": priority,
                  "percentage": percentage, "rollout_salt": salt or (old["rollout_salt"] if old and old["release_sequence"] == sequence else os.urandom(32).hex()),
                  "withdrawn": withdrawn if withdrawn is not None else (old["withdrawn"] if old else []),
                  "advisory": {"classification": "info", "summary": advisory}}
        if len(choice["withdrawn"]) > 32 or len(advisory.encode()) > 512:
            raise ValueError("channel metadata exceeds device limits")
        targets = Targets()
        self.add_target(targets, f"channels/{channel}.json", encoded(choice))
        self.metadata_for(channel, targets)

    def target_bytes(self, role, path):
        target = self.metadata[role].signed.targets[path]
        name = Path(path)
        published = "targets/" + name.with_name(target.hashes["sha256"] + "." + name.name).as_posix()
        return self.files.get(published) or object_path(self.directory, published).read_bytes()

    def channel_value(self, channel):
        return json.loads(self.target_bytes(channel, f"channels/{channel}.json"))

    def online(self):
        meta = {}
        for role in ("targets", "releases", *CHANNELS):
            metadata = self.metadata[role]
            path = f"metadata/{metadata.signed.version}.{role}.json"
            data = self.files.get(path) or (self.directory / path).read_bytes()
            meta[role + ".json"] = MetaFile.from_data(metadata.signed.version, data, ["sha256"])
        data = self.metadata_for("snapshot", Snapshot(meta=meta))
        self.metadata_for("timestamp", Timestamp(snapshot_meta=MetaFile.from_data(self.versions["snapshot"], data, ["sha256"])))

    def add_release(self, *, sequence, version, revision, image, board_family, boot_key_id, provenance, licenses, notes,
                    hardware_min=1, hardware_max=1, layout=1, minimum_updater=1):
        if sequence <= 0 or sequence >= 2**64 or len(image) > 0x400000:
            raise ValueError("release sequence or image size is outside the device profile")
        if board_family not in BOARD_FAMILIES.values():
            raise ValueError(f"unknown board family {board_family!r}")
        targets = Targets(targets=dict(self.metadata["releases"].signed.targets))
        if f"releases/{sequence}.json" in targets.targets:
            raise ValueError("release number already exists; reserve a new build number")
        selected={self.channel_value(channel)["release_sequence"] for channel in CHANNELS}
        candidates=sorted(int(Path(path).stem) for path in targets.targets if path.startswith("releases/"))
        for old in candidates:
            if len(targets.targets)+5<=32:break
            if old in selected:continue
            path=f"releases/{old}.json";manifest=json.loads(self.target_bytes("releases",path))
            for name in [path,*[manifest[key] for key in ("artifact","provenance","licenses","notes")]]:
                targets.targets.pop(name,None)
        if len(targets.targets)+5>32:raise ValueError("selected releases exhaust the bounded target index")
        base = f"artifacts/{sequence}"
        paths = {"artifact": base + ".navfeeder-esp.bin", "provenance": base + ".provenance.json",
                 "licenses": base + ".licenses.json", "notes": f"notes/{sequence}.md"}
        blobs = {"artifact": image, "provenance": provenance, "licenses": licenses, "notes": notes}
        for key in blobs:
            self.add_target(targets, paths[key], blobs[key])
        value = {"schema": 1, "release_sequence": sequence, "version": version, "build_number": sequence,
                 "source_revision": revision, "published": self.now.strftime("%Y-%m-%dT%H:%M:%SZ"),
                 "chip": "esp32s3", "board_family": board_family, "hardware_revision_min": hardware_min,
                 "hardware_revision_max": hardware_max, "partition_layout_id": layout,
                 "minimum_updater_version": minimum_updater, "length": len(image), "sha256": digest(image),
                 "secure_boot_key_id": boot_key_id, "security_version": 0, "collector_capability": "",
                 "profile": self.signers.profile, **paths,
                 **{key + "_sha256": digest(blobs[key]) for key in ("provenance", "licenses", "notes")}}
        path = f"releases/{sequence}.json"
        self.add_target(targets, path, encoded(value))
        self.metadata_for("releases", targets)
        self.set_channel("lab", sequence, path)
        self.online()
        return value

    def load(self, bootstrap=None):
        timestamp = Metadata.from_file(str(self.directory / "metadata/timestamp.json"))
        snapshot = Metadata.from_file(str(self.directory / f"metadata/{timestamp.signed.snapshot_meta.version}.snapshot.json"))
        roots = sorted(int(p.name.split(".", 1)[0]) for p in (self.directory / "metadata").glob("*.root.json"))
        self.metadata = {"timestamp": timestamp, "snapshot": snapshot,
                         "root": Metadata.from_file(str(self.directory / f"metadata/{roots[-1]}.root.json"))}
        for role in ("targets", "releases", *CHANNELS):
            version = snapshot.signed.meta[role + ".json"].version
            self.metadata[role] = Metadata.from_file(str(self.directory / f"metadata/{version}.{role}.json"))
        # Authenticate stored state before extending it. Online roles may be
        # expired when an operator renews them; expiry is enforced by clients.
        if bootstrap is None:
            raise ValueError("loading a repository requires an explicitly trusted public bootstrap root")
        root = Metadata.from_bytes(bootstrap)
        if root.signed.unrecognized_fields.get("x_navlisten_profile") != self.signers.profile:
            raise ValueError("stored repository has the wrong trust profile")
        root.verify_delegate("root", root)
        if roots[-1] < root.signed.version:
            raise ValueError("stored root precedes the trusted bootstrap")
        for version in range(root.signed.version + 1, self.metadata["root"].signed.version + 1):
            newer = Metadata.from_file(str(self.directory / f"metadata/{version}.root.json"))
            if newer.signed.version != version or newer.signed.unrecognized_fields.get("x_navlisten_profile") != self.signers.profile:
                raise ValueError("root rotation version or trust profile differs")
            root.verify_delegate("root", newer)
            newer.verify_delegate("root", newer)
            root = newer
        if root.signed.is_expired(self.now):
            raise ValueError("offline root expired; an offline renewal is required")
        self.metadata["root"] = root
        root.verify_delegate("timestamp", timestamp)
        root.verify_delegate("snapshot", snapshot)
        if snapshot.signed.version != timestamp.signed.snapshot_meta.version:
            raise ValueError("snapshot version differs from timestamp")
        timestamp.signed.snapshot_meta.verify_length_and_hashes((self.directory / f"metadata/{snapshot.signed.version}.snapshot.json").read_bytes())
        for role in ("targets", "releases", *CHANNELS):
            md = self.metadata[role]
            if md.signed.version != snapshot.signed.meta[role + ".json"].version:
                raise ValueError("delegated metadata version differs from snapshot")
            snapshot.signed.meta[role + ".json"].verify_length_and_hashes((self.directory / f"metadata/{md.signed.version}.{role}.json").read_bytes())
            parent = root if role == "targets" else self.metadata["targets"]
            parent.verify_delegate(role, md)
            if role in ("targets", "releases") and md.signed.is_expired(self.now):
                raise ValueError(f"offline {role} expired; offline renewal is required")
        self.versions = {role: md.signed.version for role, md in self.metadata.items()}

    def publish_local(self):
        paths = {path: object_path(self.directory, path) for path in self.files}
        for path, data in self.files.items():
            if path != "metadata/timestamp.json":
                immutable(paths[path], data)
        # The only replaceable object is committed last, on the same filesystem.
        atomic(self.directory / "metadata/timestamp.json", self.files["metadata/timestamp.json"])
