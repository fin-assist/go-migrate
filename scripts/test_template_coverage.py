"""Exercise the N/A lifecycle with real Go collectors and the pinned native analyzer."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[1]


class TemplateCoverageTests(unittest.TestCase):
    def run_make(self, root, target, repository="fin-assist/go-library-template", success=True):
        command = ["make", target]
        tool = os.environ.get("GO_TEST_COVERAGE_BIN")
        if tool:
            command.append("COVERAGE=" + tool)
        process = subprocess.Popen(command, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                   text=True, start_new_session=True,
                                   env=dict(os.environ, GITHUB_ACTIONS="true", GITHUB_REPOSITORY=repository))
        try:
            output, _ = process.communicate(timeout=180)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            self.fail("coverage lifecycle exceeded 180 seconds")
        self.assertEqual(process.returncode == 0, success, output)
        return output

    def test_empty_template_clone_and_first_production_function(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copyfile(REPO / "Makefile", root / "Makefile")
            shutil.copyfile(REPO / ".testcoverage.yml", root / ".testcoverage.yml")
            (root / "scripts").mkdir()
            shutil.copyfile(REPO / "scripts/check-suite.sh", root / "scripts/check-suite.sh")
            # A clone is strict even before changing the template module path.
            (root / "go.mod").write_text("module github.com/fin-assist/go-library-template\n\ngo 1.26.0\n")
            (root / "library.go").write_text("package library\nconst Name = \"template\"\n")
            (root / "library_test.go").write_text('package library\nimport "testing"\n' + "".join(
                f'func Test{level}Name(t *testing.T) {{ if Name!="template" {{ t.Fatal(Name) }} }}\n'
                for level in ["Unit", "Integration", "API"]))
            for level in ["unit", "integration", "api"]:
                self.run_make(root, "collect-" + level, repository="fin-assist/go-new", success=False)
                self.assertEqual((root / (level + ".out")).read_text(), "mode: atomic\n")
            self.run_make(root, "cover")
            for level in ["unit", "integration", "api", "combined"]:
                self.assertTrue((root / (level + ".report.txt")).read_text().startswith("N/A:"))
                self.assertFalse((root / (level + ".coverage.txt")).exists())
            # A failed/skipped suite must not inherit the empty-template exception.
            (root / "unit.jsonl").write_text('{"Action":"pass","Test":"TestUnitName"}\n'
                                             '{"Action":"skip","Test":"TestUnitSkipped"}\n')
            self.run_make(root, "check-unit", success=False)
            self.assertFalse((root / "unit.report.txt").exists())
            self.run_make(root, "cover-combined", success=False)
            # Mixed empty and nonempty profiles are inconsistent, never a combined N/A.
            self.run_make(root, "collect-unit")
            with (root / "library.go").open("a") as file:
                file.write("func Value() int { return 42 }\n")
            self.run_make(root, "collect-api")
            output = self.run_make(root, "cover-combined", success=False)
            self.assertIn("profiles disagree", output)
            self.run_make(root, "cover", success=False)
            for level in ["unit", "integration", "api"]:
                report = (root / (level + ".report.txt")).read_text()
                self.assertNotIn("N/A", report)
                self.assertIn("FAIL", report)
                self.assertTrue((root / (level + ".coverage.txt")).exists())
            # The same targets automatically enforce and then pass the native threshold.
            (root / "library_test.go").write_text('package library\nimport "testing"\n' + "".join(
                f'func Test{level}Value(t *testing.T) {{ if Value()!=42 {{ t.Fatal("value") }} }}\n'
                for level in ["Unit", "Integration", "API"]))
            self.run_make(root, "cover")
            for level in ["unit", "integration", "api"]:
                report = (root / (level + ".report.txt")).read_text()
                self.assertNotIn("N/A", report)
                self.assertIn("PASS", report)
            self.run_make(root, "check-api", repository="fin-assist/go-new")


if __name__ == "__main__":
    unittest.main()
