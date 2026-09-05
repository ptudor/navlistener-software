#!/usr/bin/env python3
"""Record the component actually selected by CMake; run in the IDF Python env."""
import hashlib
import json
from pathlib import Path
import re
import subprocess

import yaml
from idf_component_tools.hash_tools.calculate import hash_dir
from idf_component_tools.manager import ManifestManager


def git(path, *args):
    result = subprocess.run(['git', '-C', str(path), *args], capture_output=True, text=True)
    return result.stdout.strip() if result.returncode == 0 else None


def record(root):
    root = root.resolve()
    destination = root / 'build/firmware-provenance.json'
    destination.unlink(missing_ok=True)
    description_path = root / 'build/project_description.json'
    project = json.loads(description_path.read_text())
    app = root / 'build' / project['app_bin']
    # The record names one firmware image and one CMake configuration; a binary
    # older than the configuration that selected the component is some earlier
    # build's output (standalone use after `idf.py reconfigure`, say), and
    # hashing it would attribute the wrong component to that image.
    if app.stat().st_mtime < description_path.stat().st_mtime:
        raise ValueError('firmware predates the current CMake configuration; rebuild before recording provenance')
    lock_path = root / 'dependencies.lock'
    lock = yaml.safe_load(lock_path.read_text())
    pin = yaml.safe_load((root / 'main/idf_component.yml').read_text())['dependencies']['esp_hardware_discovery']
    dependency = lock['dependencies'].get('esp_hardware_discovery', {})
    component = Path(project['build_component_info']['esp_hardware_discovery']['dir']).resolve()
    managed = component == (root / 'managed_components/esp_hardware_discovery').resolve()
    manifest = ManifestManager(str(component / 'idf_component.yml'), name='esp_hardware_discovery').load()
    digest = hash_dir(component, use_gitignore=manifest.use_gitignore,
                      include=manifest.include_set, exclude=manifest.exclude_set)
    if not re.fullmatch('[0-9a-f]{40}', pin['version']):
        raise ValueError('hardware discovery must be pinned to an immutable Git commit')
    lock_matches_pin = (dependency.get('version') == pin['version'] and
                        dependency.get('source', {}).get('git') == pin['git'])
    if managed:
        if not lock_matches_pin or dependency.get('component_hash') != digest:
            raise ValueError('selected hardware discovery does not match the pinned resolution')
    else:
        print('WARNING: local hardware-discovery override; this is a development build, '
              'not a reproducible release from the committed dependency resolution.')
        if not lock_matches_pin:
            # The component manager rewrites the tracked lock for an override
            # build (type: local, a machine-specific path, no commit or content
            # hash). Committing that would discard the reviewed resolution the
            # lock exists to record.
            print('NOTE: dependencies.lock was rewritten for this override build and no longer '
                  'records the pinned Git resolution; restore it with '
                  '`git checkout -- esp32/dependencies.lock` before committing.')
    dirty = git(root, 'status', '--porcelain', '--untracked-files=normal')
    result = {
        'schema': 1,
        'project_revision': git(root, 'rev-parse', 'HEAD'),
        # None, not False, when the tree is not a Git checkout: "unknown" must
        # not read as "clean".
        'project_dirty': None if dirty is None else bool(dirty),
        'idf_revision': project['git_revision'],
        'target': project['target'],
        'dependencies_lock_sha256': hashlib.sha256(lock_path.read_bytes()).hexdigest(),
        'dependencies_lock_matches_pin': lock_matches_pin,
        'hardware_discovery': {
            'source': pin['git'] if managed else str(component),
            'commit': dependency['version'] if managed else git(component, 'rev-parse', 'HEAD'),
            'component_hash': digest,
            'local_override': not managed,
            'matches_manifest_pin': managed,
        },
        'firmware_sha256': hashlib.sha256(app.read_bytes()).hexdigest(),
    }
    destination.write_text(json.dumps(result, indent=2) + '\n')
    print('Firmware provenance:', destination)
    return result


if __name__ == '__main__':
    record(Path(__file__).resolve().parents[1])
