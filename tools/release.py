"""Publish a tested SHA image. CI must serialize main runs without cancellation."""

import os
from pathlib import Path
import re
import subprocess


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


def publish(repository: str, sha: str, base: str) -> None:
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

    # Reserve the version before touching registry tags so failures cannot reuse it.
    if tag not in tags:
        run(
            "gh", "api", f"{api}/git/refs", "--method", "POST",
            "-f", f"ref=refs/tags/{tag}", "-f", f"sha={sha}",
        )
    image = f"ghcr.io/{repository.lower()}"
    run(
        "docker", "buildx", "imagetools", "create",
        "--tag", f"{image}:{version}", "--tag", f"{image}:latest",
        f"{image}:sha-{sha}",
    )
    run(
        "gh", "release", "create", tag, "--repo", repository,
        "--verify-tag", "--title", tag, "--generate-notes", "--latest",
        "--notes", f"Container: `{image}:{version}`\n\nCommit: `{sha}`",
    )
    print(f"Published {tag}: {image}:{version}")


if __name__ == "__main__":
    publish(
        os.environ["GITHUB_REPOSITORY"],
        os.environ["GITHUB_SHA"],
        Path("VERSION").read_text().strip(),
    )
