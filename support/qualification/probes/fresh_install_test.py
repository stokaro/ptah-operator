from pathlib import Path
import unittest

from fresh_install import blocks, substitute

DOCS = Path(__file__).resolve().parents[3] / 'docs/site/src/content/docs'


class BlockExtractionTest(unittest.TestCase):
    def test_a_section_ends_at_a_heading_of_its_level_and_keeps_its_subsections(self):
        page = '\n'.join([
            '## First', '```sh', 'one', '```', '### Inside', '```sh', 'two', '```',
            '```yaml', 'not: shell', '```', '## Second {#second}', '```sh', 'three', '```',
        ])
        self.assertEqual(blocks(page, 'First'), ['one\n', 'two\n'])
        self.assertEqual(blocks(page, 'Second'), ['three\n'])
        with self.assertRaises(ValueError):
            blocks(page, 'Missing')

    def test_a_heading_inside_a_fence_is_not_a_heading(self):
        page = '\n'.join(['## Real', '```sh', '## not a heading', 'echo kept', '```'])
        self.assertEqual(blocks(page, 'Real'), ['## not a heading\necho kept\n'])

    def test_substitution_refuses_a_placeholder_the_guide_lost_or_repeats(self):
        self.assertEqual(substitute('a=<x>\n', [('<x>', '1')]), 'a=1\n')
        with self.assertRaises(ValueError):
            substitute('a=1\n', [('<x>', '1')])
        with self.assertRaises(ValueError):
            substitute('<x> <x>\n', [('<x>', '1')])


class PublishedGuideTest(unittest.TestCase):
    """The walkthrough reads these sections; a renamed heading or a reworded
    placeholder must fail here rather than on a cluster."""

    def read(self, page):
        return (DOCS / page).read_text()

    def test_the_release_guide_verifies_then_installs_in_one_shell(self):
        page = self.read('support/releases.md')
        verify = ''.join(blocks(page, 'Verify before installation'))
        install = ''.join(blocks(page, 'Install by digest'))
        self.assertIn('tag=v0.2.0\n', verify)
        for variable in ('image', 'executor', 'ptah_version', 'version'):
            self.assertIn(f'{variable}=', verify)
        self.assertIn('helm upgrade --install ptah-operator', install)
        self.assertIn('${image##*@}', install)

    def test_the_install_guide_confirms_the_installation(self):
        confirm = ''.join(blocks(self.read('start/install.md'), 'Confirm it installed'))
        self.assertIn('wait --for=condition=Available deployment --all', confirm)
        self.assertIn('Established', confirm)

    def test_the_first_schema_guide_has_the_blocks_the_walkthrough_runs(self):
        page = self.read('start/first-schema.md')
        create = blocks(page, 'Create what the schema reads')
        self.assertEqual(len(create), 3)
        substitute(create[0], [('DATABASE_URL_FILE=/path/to/private/database-url\n', 'DATABASE_URL_FILE=x\n')])
        self.assertIn('kubectl apply -f examples/ptahschema.yaml\n', create[1])
        self.assertIn('kubectl apply -f examples/ptahschema-mysql.yaml\n', create[2])
        for block in create[1:]:
            self.assertIn('kubectl -n application get ptahschema application -w', block)
        stops = blocks(page, 'It stops, and waits for you')
        self.assertEqual(len(stops), 4)
        self.assertTrue(any('kubectl -n application create -f -' in block for block in stops))

    def test_the_plugin_install_has_the_placeholders_the_walkthrough_fills(self):
        block = ''.join(blocks(self.read('use/read-a-plan.md'), 'Install it'))
        substitute(block, [
            ('version=<release tag>\n', 'version=v0.2.0\n'),
            ('platform=darwin-arm64   # or linux-amd64, linux-arm64, darwin-amd64\n', 'platform=linux-amd64\n'),
            ('/usr/local/bin/kubectl-ptah', '/tmp/bin/kubectl-ptah'),
        ])

    def test_each_example_has_one_artifact_placeholder_and_one_policy_reference(self):
        examples = Path(__file__).resolve().parents[3] / 'examples'
        for name in ('ptahschema.yaml', 'ptahschema-mysql.yaml'):
            text = (examples / name).read_text()
            self.assertEqual(text.count('@sha256:<digest>'), 1, name)
            self.assertEqual(text.count('    verificationPolicyFrom:\n'), 1, name)


if __name__ == '__main__':
    unittest.main()
