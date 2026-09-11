from __future__ import annotations

import contextlib
import hashlib
import importlib.util
import io
import os
import stat
import tempfile
import unittest
from pathlib import Path


SCRIPT_PATH = Path(__file__).with_name("stage_ghd_release_assets.py")
SPEC = importlib.util.spec_from_file_location("stage_ghd_release_assets", SCRIPT_PATH)
assert SPEC is not None
assert SPEC.loader is not None
stage_ghd_release_assets = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(stage_ghd_release_assets)


PLATFORMS = (
    ("darwin", "amd64"),
    ("darwin", "arm64"),
    ("linux", "amd64"),
    ("linux", "arm64"),
)


@contextlib.contextmanager
def working_directory(path: Path):
    original = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(original)


@contextlib.contextmanager
def github_repository(value: str):
    original = os.environ.get("GITHUB_REPOSITORY")
    os.environ["GITHUB_REPOSITORY"] = value
    try:
        yield
    finally:
        if original is None:
            os.environ.pop("GITHUB_REPOSITORY", None)
        else:
            os.environ["GITHUB_REPOSITORY"] = original


class StageGhdReleaseAssetsTest(unittest.TestCase):
    def test_stages_expected_assets(self) -> None:
        with fixture() as root:
            result, stdout, stderr = run_script(root)

            self.assertEqual(result, 0, stderr)
            staged = sorted(path.name for path in (root / "dist/release-assets").iterdir())
            self.assertEqual(
                staged,
                [
                    "checksums.txt",
                    "template-mcp_1.2.3_darwin_amd64",
                    "template-mcp_1.2.3_darwin_amd64.sbom.json",
                    "template-mcp_1.2.3_darwin_arm64",
                    "template-mcp_1.2.3_darwin_arm64.sbom.json",
                    "template-mcp_1.2.3_linux_amd64",
                    "template-mcp_1.2.3_linux_amd64.sbom.json",
                    "template-mcp_1.2.3_linux_arm64",
                    "template-mcp_1.2.3_linux_arm64.sbom.json",
                ],
            )
            linux_binary = root / "dist/release-assets/template-mcp_1.2.3_linux_amd64"
            mode = linux_binary.stat().st_mode
            self.assertTrue(mode & stat.S_IXUSR)
            self.assertIn("dist/release-assets/template-mcp_1.2.3_linux_arm64", stdout)

    def test_fails_on_missing_checksum_entry(self) -> None:
        with fixture(missing_checksum="template-mcp_1.2.3_linux_arm64") as root:
            result, _, stderr = run_script(root)

            self.assertEqual(result, 1)
            self.assertIn("missing checksum entry", stderr)
            self.assertIn("template-mcp_1.2.3_linux_arm64", stderr)

    def test_fails_on_checksum_mismatch(self) -> None:
        override = ("template-mcp_1.2.3_linux_amd64", "0" * 64)
        with fixture(checksum_override=override) as root:
            result, _, stderr = run_script(root)

            self.assertEqual(result, 1)
            self.assertIn("checksum mismatch for template-mcp_1.2.3_linux_amd64", stderr)

    def test_fails_on_wrong_signer_workflow(self) -> None:
        with fixture(signer="other/repo/.github/workflows/attest.yml") as root:
            result, _, stderr = run_script(root)

            self.assertEqual(result, 1)
            self.assertIn("signer_workflow", stderr)

    def test_fails_on_missing_os_arch_asset(self) -> None:
        with fixture(omit_artifact=("linux", "arm64", "Binary")) as root:
            result, _, stderr = run_script(root)

            self.assertEqual(result, 1)
            self.assertIn("missing expected binary asset template-mcp_1.2.3_linux_arm64", stderr)

    def test_fails_on_unexpected_asset_count(self) -> None:
        with fixture(extra_binary=True) as root:
            result, _, stderr = run_script(root)

            self.assertEqual(result, 1)
            self.assertIn("expected 9 draft release assets, found 10", stderr)


def run_script(root: Path) -> tuple[int, str, str]:
    stdout = io.StringIO()
    stderr = io.StringIO()
    with working_directory(root), github_repository("meigma/template-mcp"):
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = stage_ghd_release_assets.main(["--tag", "v1.2.3"])
    return result, stdout.getvalue(), stderr.getvalue()


@contextlib.contextmanager
def fixture(
    *,
    signer: str = "meigma/template-mcp/.github/workflows/attest.yml",
    missing_checksum: str | None = None,
    checksum_override: tuple[str, str] | None = None,
    omit_artifact: tuple[str, str, str] | None = None,
    extra_binary: bool = False,
):
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        (root / "dist").mkdir()
        write_ghd_toml(root / "ghd.toml", signer)

        artifacts: list[dict[str, str]] = []
        checksum_entries: dict[str, str] = {}
        for goos, goarch in PLATFORMS:
            binary_name = f"template-mcp_1.2.3_{goos}_{goarch}"
            sbom_name = f"{binary_name}.sbom.json"

            binary_path = root / "dist" / binary_name
            binary_path.write_bytes(f"{binary_name}\n".encode())
            checksum_entries[binary_name] = sha256(binary_path)
            if omit_artifact != (goos, goarch, "Binary"):
                artifacts.append({
                    "type": "Binary",
                    "name": binary_name,
                    "path": f"dist/{binary_name}",
                })

            sbom_path = root / "dist" / sbom_name
            sbom_path.write_text(f'{{"name": "{sbom_name}"}}\n', encoding="utf-8")
            if omit_artifact != (goos, goarch, "SBOM"):
                artifacts.append({
                    "type": "SBOM",
                    "name": binary_name,
                    "path": f"dist/{sbom_name}",
                })

        if extra_binary:
            extra_name = "template-mcp_1.2.3_freebsd_amd64"
            extra_path = root / "dist" / extra_name
            extra_path.write_bytes(b"extra\n")
            artifacts.append({"type": "Binary", "name": extra_name, "path": f"dist/{extra_name}"})

        if checksum_override is not None:
            checksum_entries[checksum_override[0]] = checksum_override[1]
        if missing_checksum is not None:
            checksum_entries.pop(missing_checksum)

        checksums_path = root / "dist/checksums.txt"
        checksums_path.write_text(
            "".join(f"{digest}  {name}\n" for name, digest in sorted(checksum_entries.items())),
            encoding="utf-8",
        )
        artifacts.append({
            "type": "Checksum",
            "name": "checksums.txt",
            "path": "dist/checksums.txt",
        })

        artifacts_path = root / "dist/artifacts.json"
        artifacts_path.write_text(format_artifacts_json(artifacts), encoding="utf-8")
        yield root


def write_ghd_toml(path: Path, signer: str) -> None:
    path.write_text(
        f'''version = 1

[provenance]
signer_workflow = "{signer}"

[[packages]]
name = "template-mcp"
description = "Meigma Go MCP server template starter CLI."
tag_pattern = "v${{version}}"

[[packages.assets]]
os = "darwin"
arch = "amd64"
pattern = "template-mcp_${{version}}_darwin_amd64"

[[packages.assets]]
os = "darwin"
arch = "arm64"
pattern = "template-mcp_${{version}}_darwin_arm64"

[[packages.assets]]
os = "linux"
arch = "amd64"
pattern = "template-mcp_${{version}}_linux_amd64"

[[packages.assets]]
os = "linux"
arch = "arm64"
pattern = "template-mcp_${{version}}_linux_arm64"

[[packages.binaries]]
path = "template-mcp"
''',
        encoding="utf-8",
    )


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def format_artifacts_json(artifacts: list[dict[str, str]]) -> str:
    lines = ["[\n"]
    for index, artifact in enumerate(artifacts):
        suffix = "," if index < len(artifacts) - 1 else ""
        fields = ", ".join(f'"{key}": "{value}"' for key, value in artifact.items())
        lines.append(f"  {{{fields}}}{suffix}\n")
    lines.append("]\n")
    return "".join(lines)


if __name__ == "__main__":
    unittest.main()
