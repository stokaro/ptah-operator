import json
from pathlib import Path
import tempfile
import unittest

from release_scan import decode_stream, govulncheck_findings, manifest_fields, trivy_findings

STREAM = b'''{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck"}}
{"osv":{"id":"GO-2026-0001"}}
{"finding":{"osv":"GO-2026-0001","trace":[{"module":"golang.org/x/net","package":"golang.org/x/net/http2","function":"Server.ServeConn"}]}}
{"finding":{"osv":"GO-2026-0002","trace":[{"module":"golang.org/x/crypto","package":"golang.org/x/crypto/openpgp"}]}}
{"finding":{"osv":"GO-2026-0003","trace":[{"module":"example.com/m"}]}}
{"finding":{"osv":"GO-2026-0001","trace":[{"module":"golang.org/x/net","package":"golang.org/x/net/http2"}]}}
'''


class ReleaseScanTest(unittest.TestCase):
    def test_a_finding_counts_as_reached_only_when_its_trace_reaches_a_function(self):
        reached, imported = govulncheck_findings(decode_stream(STREAM))
        self.assertEqual(reached, ['GO-2026-0001'])
        self.assertEqual(imported, ['GO-2026-0002', 'GO-2026-0003'])

    def test_an_empty_scan_reports_nothing(self):
        self.assertEqual(govulncheck_findings(decode_stream(b'{"config":{}}\n')), ([], []))

    def test_trivy_findings_are_read_from_every_result(self):
        report = {'Results': [{'Vulnerabilities': [{'VulnerabilityID': 'CVE-2026-2'}]}, {'Vulnerabilities': None},
                              {'Vulnerabilities': [{'VulnerabilityID': 'CVE-2026-1'}, {'VulnerabilityID': 'CVE-2026-2'}]}]}
        self.assertEqual(trivy_findings(report), ['CVE-2026-1', 'CVE-2026-2'])
        self.assertEqual(trivy_findings({}), [])

    def test_the_manifest_must_name_pinned_images_once(self):
        good = ('source-sha=' + 'a' * 40 + '\nimage=ghcr.io/stokaro/ptah-operator@sha256:' + 'b' * 64 +
                '\nexecutor=ghcr.io/stokaro/ptah-operator-executor@sha256:' + 'c' * 64 + '\nexecutor-ptah-commit=' + 'd' * 40 + '\n')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'release-manifest.txt'
            path.write_text(good)
            self.assertEqual(manifest_fields(path)['source-sha'], 'a' * 40)
            for broken in (good.replace('@sha256:' + 'b' * 64, ':v0.2.0'), good + 'image=again\n',
                           good.replace('executor-ptah-commit=' + 'd' * 40 + '\n', '')):
                path.write_text(broken)
                with self.assertRaises(ValueError):
                    manifest_fields(path)


if __name__ == '__main__':
    unittest.main()
