"""Execution-evidence guard tests; no coverage calculation is implemented here."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


GUARD = Path(__file__).with_name("check-suite.sh").resolve()


class SuiteInputsTests(unittest.TestCase):
    def check(self, events, profile, expected, repository="fin-assist/go-example", origin=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / "suite.jsonl"
            log.write_text("".join(json.dumps(event) + "\n" for event in events))
            coverage = root / "suite.out"
            if profile is not None:
                coverage.write_text(profile)
            if origin is not None:
                subprocess.run(["git", "init", "-q", str(root)], check=True, timeout=10)
                subprocess.run(["git", "-C", str(root), "remote", "add", "origin", origin],
                               check=True, timeout=10)
            env = dict(os.environ, GITHUB_ACTIONS="true", GITHUB_REPOSITORY=repository)
            if origin is not None:
                env.pop("GITHUB_ACTIONS")
                env.pop("GITHUB_REPOSITORY")
            result = subprocess.run(
                ["bash", str(GUARD), str(log), str(coverage)],
                cwd=root, env=env, capture_output=True, text=True, timeout=10,
            )
            self.assertEqual(result.returncode == 0, expected, result.stderr)
            return result.stdout.strip()

    def test_executed_test_and_packages_without_tests(self):
        self.check([
            {"Action": "pass", "Test": "TestUnitRule"},
            {"Action": "skip", "Package": "example.com/service/domain"},
        ], "mode: atomic\nexample.com/service/rule.go:3.1,3.20 1 0\n", True)

    def test_package_pass_does_not_prove_test_execution(self):
        self.check([{"Action": "pass", "Package": "example.com/service"}],
                   "mode: atomic\nexample.com/service/rule.go:3.1,3.20 1 1\n", False)

    def test_empty_event_stream(self):
        self.check([], "mode: atomic\nexample.com/service/rule.go:3.1,3.20 1 1\n", False)

    def test_skipped_or_failed_test_is_not_coverage_evidence(self):
        for action in ["skip", "fail"]:
            with self.subTest(action=action):
                self.check([
                    {"Action": "pass", "Test": "TestUnitRule"},
                    {"Action": action, "Test": "TestUnitDatabase"},
                ], "mode: atomic\nexample.com/service/rule.go:3.1,3.20 1 1\n", False)

    def test_missing_empty_header_only_and_zero_statement_profiles(self):
        for profile in [None, "", "mode: atomic\n",
                        "mode: atomic\nexample.com/service/rule.go:3.1,3.20 0 1\n"]:
            with self.subTest(profile=profile):
                self.check([{"Action": "pass", "Test": "TestUnitRule"}], profile, False)

    def test_malformed_execution_log(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "suite.jsonl"
            log.write_text("not JSON\n")
            result = subprocess.run(["bash", str(GUARD), str(log), str(log)],
                                    capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)

    def test_only_original_template_can_report_na(self):
        passed = [{"Action": "pass", "Test": "TestUnitRule"}]
        self.assertEqual(self.check(passed, "mode: atomic\n", True,
                                    repository="fin-assist/go-library-template"), "N/A")
        for repository in ["", "fin-assist/go-example", "someone/go-library-template",
                           "fin-assist/go-library-template-copy"]:
            with self.subTest(repository=repository):
                self.check(passed, "mode: atomic\n", False, repository=repository)
        # GitHub identity wins over a stale template origin in a newly created repository.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", str(root)], check=True, timeout=10)
            subprocess.run(["git", "-C", str(root), "remote", "add", "origin",
                            "https://github.com/fin-assist/go-library-template.git"], check=True, timeout=10)
            (root / "suite.jsonl").write_text(json.dumps(passed[0]) + "\n")
            (root / "suite.out").write_text("mode: atomic\n")
            result = subprocess.run(["bash", str(GUARD), "suite.jsonl", "suite.out"], cwd=root,
                                    env=dict(os.environ, GITHUB_ACTIONS="true", GITHUB_REPOSITORY="fin-assist/go-new"),
                                    capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)

    def test_local_original_remote_and_clone(self):
        passed = [{"Action": "pass", "Test": "TestUnitRule"}]
        for origin in ["https://github.com/fin-assist/go-library-template.git",
                       "https://github.com/fin-assist/go-library-template",
                       "git@github.com:fin-assist/go-library-template.git",
                       "ssh://git@github.com/fin-assist/go-library-template.git"]:
            with self.subTest(origin=origin):
                self.assertEqual(self.check(passed, "mode: atomic\n", True, origin=origin), "N/A")
        for origin in ["https://github.com/fin-assist/go-new.git",
                       "https://github.com/someone/go-library-template.git",
                       "https://github.com/fin-assist/go-library-template.git/extra"]:
            with self.subTest(origin=origin):
                self.check(passed, "mode: atomic\n", False, origin=origin)

    def test_na_does_not_accept_bad_profile_or_suite(self):
        passed = [{"Action": "pass", "Test": "TestUnitRule"}]
        for profile in [None, "", "mode: count\n", "mode: atomic", "mode: atomic\n\n",
                        "mode: atomic\ngarbage\n", "mode: atomic\nx.go:1.1,1.2 0 1\n"]:
            with self.subTest(profile=profile):
                self.check(passed, profile, False, repository="fin-assist/go-library-template")
        for events in [[], [{"Action": "pass", "Package": "template"}],
                       passed + [{"Action": "skip", "Test": "TestUnitSkipped"}],
                       passed + [{"Action": "fail", "Test": "TestUnitFailed"}],
                       passed + [{"Action": "fail", "Package": "template"}]]:
            with self.subTest(events=events):
                self.check(events, "mode: atomic\n", False, repository="fin-assist/go-library-template")

    def test_production_statements_never_report_na(self):
        self.assertEqual(self.check([{"Action": "pass", "Test": "TestUnitRule"}],
                                    "mode: atomic\nx.go:1.1,1.2 1 0\n", True,
                                    repository="fin-assist/go-library-template"), "statements")


if __name__ == "__main__":
    unittest.main()
