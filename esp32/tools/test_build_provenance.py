"""Provenance checks against disposable CMake/component fixtures (IDF Python)."""
import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import tempfile
import time
import unittest
import yaml

from build_provenance import record, hash_dir


class ProvenanceTests(unittest.TestCase):
    def test_pinned_content_and_actual_override(self):
        source = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'build').mkdir()
            (root / 'main').mkdir()
            shutil.copy(source / 'main/idf_component.yml', root / 'main/idf_component.yml')
            shutil.copy(source / 'dependencies.lock', root / 'dependencies.lock')
            component = root / 'managed_components/esp_hardware_discovery'
            component.mkdir(parents=True)
            (component / 'idf_component.yml').write_text('version: "1.0.0"\n')
            (component / 'source.c').write_text('original\n')
            lock = yaml.safe_load((root / 'dependencies.lock').read_text())
            lock['dependencies']['esp_hardware_discovery']['component_hash'] = hash_dir(component)
            (root / 'dependencies.lock').write_text(yaml.safe_dump(lock))
            description = dict(git_revision='v5.5.4', target='esp32c6', app_bin='app.bin',
                               build_component_info={'esp_hardware_discovery': {'dir': str(component)}})
            (root / 'build/project_description.json').write_text(json.dumps(description))
            (root / 'build/app.bin').write_bytes(b'firmware')
            with contextlib.redirect_stdout(io.StringIO()):
                first = record(root)
                second = record(root)
            self.assertEqual(first, second)
            self.assertFalse(first['hardware_discovery']['local_override'])
            self.assertEqual(first['hardware_discovery']['commit'],
                             lock['dependencies']['esp_hardware_discovery']['version'])
            (component / 'source.c').write_text('unreviewed change\n')
            with self.assertRaises(ValueError):
                record(root)
            self.assertFalse((root / 'build/firmware-provenance.json').exists())
            override = root / 'local/esp_hardware_discovery'
            shutil.copytree(component, override)
            description['build_component_info']['esp_hardware_discovery']['dir'] = str(override)
            (root / 'build/project_description.json').write_text(json.dumps(description))
            # A successful override build produces its firmware after reconfiguration.
            (root / 'build/app.bin').write_bytes(b'override firmware')
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                local = record(root)
            self.assertTrue(local['hardware_discovery']['local_override'])
            self.assertFalse(local['hardware_discovery']['matches_manifest_pin'])
            self.assertTrue(local['dependencies_lock_matches_pin'])
            self.assertIn('development build', output.getvalue())
            self.assertNotIn('dependencies.lock was rewritten', output.getvalue())
            self.assertEqual(local['hardware_discovery']['source'], str(override.resolve()))
            # The component manager rewrites the tracked lock for an override build;
            # the record says so and tells the developer how to restore it.
            rewritten = dict(lock)
            rewritten['dependencies']['esp_hardware_discovery'] = {
                'source': {'path': str(override), 'type': 'local'}, 'version': '1.0.0'}
            (root / 'dependencies.lock').write_text(yaml.safe_dump(rewritten))
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                local = record(root)
            self.assertFalse(local['dependencies_lock_matches_pin'])
            self.assertIn('git checkout -- esp32/dependencies.lock', output.getvalue())

    def fixture(self, root, **overrides):
        source = Path(__file__).resolve().parents[1]
        (root / 'build').mkdir()
        (root / 'main').mkdir()
        shutil.copy(source / 'main/idf_component.yml', root / 'main/idf_component.yml')
        shutil.copy(source / 'dependencies.lock', root / 'dependencies.lock')
        component = root / 'managed_components/esp_hardware_discovery'
        component.mkdir(parents=True)
        (component / 'idf_component.yml').write_text('version: "1.0.0"\n')
        (component / 'source.c').write_text('original\n')
        lock = yaml.safe_load((root / 'dependencies.lock').read_text())
        lock['dependencies']['esp_hardware_discovery']['component_hash'] = hash_dir(component)
        lock['dependencies']['esp_hardware_discovery'].update(overrides.get('lock', {}))
        (root / 'dependencies.lock').write_text(yaml.safe_dump(lock))
        if 'pin' in overrides:
            manifest = yaml.safe_load((root / 'main/idf_component.yml').read_text())
            manifest['dependencies']['esp_hardware_discovery']['version'] = overrides['pin']
            (root / 'main/idf_component.yml').write_text(yaml.safe_dump(manifest))
        description = dict(git_revision='v5.5.4', target='esp32c6', app_bin='app.bin',
                           build_component_info={'esp_hardware_discovery': {'dir': str(component)}})
        (root / 'build/project_description.json').write_text(json.dumps(description))
        (root / 'build/app.bin').write_bytes(b'firmware')
        return root

    def test_moving_branch_pin_is_rejected(self):
        # The original finding: a `main` (or tag) pin resolves different code per build.
        for pin in ('main', 'v1.2.0', '5d7e533'):
            with tempfile.TemporaryDirectory() as temp:
                root = self.fixture(Path(temp), pin=pin)
                with self.assertRaises(ValueError), contextlib.redirect_stdout(io.StringIO()):
                    record(root)
                self.assertFalse((root / 'build/firmware-provenance.json').exists())

    def test_lock_that_disagrees_with_the_pin_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.fixture(Path(temp), lock={'version': '720595f1dfb8cae61c2c00dfea5f8279a003e732'})
            with self.assertRaises(ValueError), contextlib.redirect_stdout(io.StringIO()):
                record(root)
            self.assertFalse((root / 'build/firmware-provenance.json').exists())

    def test_firmware_older_than_configuration_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.fixture(Path(temp))
            stale = time.time() - 600
            os.utime(root / 'build/app.bin', (stale, stale))
            with self.assertRaises(ValueError), contextlib.redirect_stdout(io.StringIO()):
                record(root)
            self.assertFalse((root / 'build/firmware-provenance.json').exists())
            fresh = time.time() + 5
            os.utime(root / 'build/app.bin', (fresh, fresh))
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertIn('firmware_sha256', record(root))


if __name__ == '__main__':
    unittest.main()
