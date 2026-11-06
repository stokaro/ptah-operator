import unittest
from pathlib import Path

from fresh_install import blocks, installs_from_registry, recorded_identity_findings

DOCS = Path(__file__).resolve().parents[3] / 'docs/site/src/content/docs'

IDENTITIES = {
    'image': 'ghcr.io/stokaro/ptah-operator@sha256:' + '1' * 64,
    'executor': 'ghcr.io/stokaro/ptah@sha256:' + '2' * 64,
    'ptah_version': 'v0.18.0',
}


def status():
    return {
        'executionBinding': {'executorImage': IDENTITIES['executor'], 'ptahVersion': IDENTITIES['ptah_version']},
        'applied': {'executorImage': IDENTITIES['executor'], 'ptahVersion': IDENTITIES['ptah_version'],
                    'controllerImage': IDENTITIES['image'], 'runnerImage': IDENTITIES['image']},
    }


class RegistryInstallTest(unittest.TestCase):
    def test_the_release_guide_installs_the_digest_it_verified_with_no_values(self):
        page = (DOCS / 'support/releases.md').read_text()
        verify = ''.join(blocks(page, 'Verify before installation'))
        for variable in ('image', 'executor', 'ptah_version', 'source_ref', 'source_sha', 'identity', 'repository'):
            self.assertIn(f'{variable}=', verify, variable)
        install = ''.join(blocks(page, '#install-from-the-registry'))
        self.assertIn('helm upgrade --install ptah-operator "oci://$chart"', install)
        self.assertNotIn('--set', install)
        self.assertNotIn('--values', install)

    def test_only_a_fresh_postgresql_walkthrough_of_a_guide_that_has_the_section_takes_it(self):
        guide = '## Install from the registry {#install-from-the-registry}\n'
        self.assertTrue(installs_from_registry(guide, 'postgresql', None))
        for releases, engine, predecessor in ((guide, 'mysql', None), (guide, None, None), (guide, 'postgresql', 'v0.2.9'),
                                              ('## Install by digest\n', 'postgresql', None)):
            self.assertFalse(installs_from_registry(releases, engine, predecessor), (engine, predecessor))

    def test_a_schema_that_records_the_release_has_no_findings(self):
        self.assertEqual(recorded_identity_findings(status(), IDENTITIES), [])

    def test_a_schema_that_records_anything_else_is_a_finding(self):
        for section, field in (('executionBinding', 'executorImage'), ('executionBinding', 'ptahVersion'),
                               ('applied', 'executorImage'), ('applied', 'ptahVersion'),
                               ('applied', 'controllerImage'), ('applied', 'runnerImage')):
            reading = status()
            reading[section][field] = 'something else'
            findings = recorded_identity_findings(reading, IDENTITIES)
            self.assertEqual(len(findings), 1, (section, field))
            self.assertIn(f'{section}.{field}', findings[0])
        self.assertEqual(len(recorded_identity_findings({}, IDENTITIES)), 6)


if __name__ == '__main__':
    unittest.main()
