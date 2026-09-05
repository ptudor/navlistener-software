"""Provenance checks against disposable CMake/component fixtures (IDF Python)."""
import contextlib
import io
import json
from pathlib import Path
import shutil
import tempfile
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
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                local = record(root)
            self.assertTrue(local['hardware_discovery']['local_override'])
            self.assertFalse(local['hardware_discovery']['matches_manifest_pin'])
            self.assertIn('development build', output.getvalue())
            self.assertEqual(local['hardware_discovery']['source'], str(override.resolve()))


if __name__ == '__main__':
    unittest.main()
