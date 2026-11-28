"""Publish a tested SHA image. CI must serialize main runs without cancellation."""

import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = ("amd64", "arm64")
DOCS = ("LICENSE", "NOTICE", "README.md", "THIRD_PARTY.md")


SEMVER = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def select_version(base: str, tags: dict[str, str], sha: str) -> str:
    match = SEMVER.fullmatch(base)
    if not match or match[3] != "0":
        raise ValueError("VERSION must contain MAJOR.MINOR.0 without leading zeroes")
    major, minor, _ = map(int, match.groups())
    versions = {
        tuple(map(int, match.groups())): commit
        for tag, commit in tags.items()
        if tag.startswith("v") and (match := SEMVER.fullmatch(tag[1:]))
    }
    if any(version[:2] > (major, minor) for version in versions):
        raise ValueError("VERSION must not go backwards from an existing release series")
    patches = {
        version[2]: commit
        for version, commit in versions.items()
        if version[:2] == (major, minor)
    }
    existing = [patch for patch, commit in patches.items() if commit == sha]
    patch = max(existing) if existing else max(patches, default=0) + 1
    return f"{major}.{minor}.{patch}"


def run(*args: str) -> str:
    return subprocess.check_output(args, text=True).strip()


def package_binaries(image: str, version: str, out: Path) -> list[Path]:
    """Extract the tested image's static executables into per-platform archives."""
    archives = []
    for arch in PLATFORMS:
        name = f"keiryo_{version}_linux_{arch}"
        stage = out / name
        stage.mkdir(parents=True)
        container = run("docker", "create", "--platform", f"linux/{arch}", image)
        try:
            for path in ("/monitoring", "/monitoring-agent", "/licenses"):
                run("docker", "cp", f"{container}:{path}", str(stage))
        finally:
            run("docker", "rm", container)
        for doc in DOCS:
            shutil.copy(ROOT / doc, stage)
        archive = out / f"{name}.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            tar.add(stage, arcname=name)
        archives.append(archive)
    sums = out / "SHA256SUMS"
    sums.write_text("".join(
        f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n"
        for archive in archives
    ))
    return [*archives, sums]


def publish(repository: str, sha: str, base: str) -> None:
    with tempfile.TemporaryDirectory(prefix="keiryo-release-") as directory:
        _publish(repository, sha, base, Path(directory))


def _publish(repository: str, sha: str, base: str, workdir: Path) -> None:
    api = f"repos/{repository}"
    head = run("gh", "api", f"{api}/git/ref/heads/main", "--jq", ".object.sha")
    if head != sha:
        print("Skipping release: commit is no longer main HEAD.")
        return

    rows = run(
        "gh", "api", f"{api}/tags", "--paginate",
        "--jq", '.[] | [.name, .commit.sha] | @tsv',
    )
    tags = dict(row.split("\t") for row in rows.splitlines())
    version = select_version(base, tags, sha)
    tag = f"v{version}"
    releases = run(
        "gh", "api", f"{api}/releases", "--paginate",
        "--jq", ".[] | select(.draft == false) | .tag_name",
    ).splitlines()
    if tag in releases:
        print(f"Skipping release: {tag} is already published.")
        return

    image = f"ghcr.io/{repository.lower()}"
    # Package before reserving the version: extraction is read-only, so a failure
    # here leaves no tag behind.
    assets = package_binaries(f"{image}:sha-{sha}", version, workdir)

    # Reserve the version before touching registry tags so failures cannot reuse it.
    if tag not in tags:
        run(
            "gh", "api", f"{api}/git/refs", "--method", "POST",
            "-f", f"ref=refs/tags/{tag}", "-f", f"sha={sha}",
        )
    run(
        "docker", "buildx", "imagetools", "create",
        "--tag", f"{image}:{version}", "--tag", f"{image}:latest",
        f"{image}:sha-{sha}",
    )
    run(
        "gh", "release", "create", tag, *map(str, assets), "--repo", repository,
        "--verify-tag", "--title", tag, "--generate-notes", "--latest",
        "--notes",
        f"Container: `{image}:{version}`\n\n"
        "Static Linux binaries (amd64/arm64) are attached; verify with `SHA256SUMS`.\n\n"
        f"Commit: `{sha}`",
    )
    print(f"Published {tag}: {image}:{version}")


if __name__ == "__main__":
    publish(
        os.environ["GITHUB_REPOSITORY"],
        os.environ["GITHUB_SHA"],
        Path("VERSION").read_text().strip(),
    )
