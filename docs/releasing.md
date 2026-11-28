# Releasing keiryō

Successful `main` builds publish multi-architecture images to
`ghcr.io/connor-ryan-1998/keiryo`. CI assigns patch versions starting at
`0.0.1`, creates matching Git tags (`v0.0.1`) and GitHub Releases with
generated release notes, and promotes the same image to `latest`.
Commit-specific `sha-<commit>` images remain available for traceability.
Pull requests only test and build; they do not publish.

Each release also attaches static Linux binaries extracted from that same tested
image: `keiryo_<version>_linux_amd64.tar.gz` and `keiryo_<version>_linux_arm64.tar.gz`,
each containing `monitoring`, `monitoring-agent`, `LICENSE`, `NOTICE`, `README.md`,
`THIRD_PARTY.md` and dependency notices under `licenses/`, plus a `SHA256SUMS` file.
Archives are built before the version tag is reserved, so a packaging failure
leaves no tag behind.

`VERSION` sets the manually managed release series. Keep its patch field at
zero: change `0.0.0` to `0.1.0` for a new minor series or `1.0.0` for a new
major series. CI publishes `0.1.1` or `1.0.1` next, respectively. Do not
decrease the series or manually increment the patch field.

Main workflow runs are serialized without automatic cancellation. Superseded
commits skip release publication. Rerunning a completed release does not move
its numbered image or `latest`. A version is reserved with a Git tag after
the image build and binary packaging succeed, before registry promotion. If publication fails,
rerun the failed release job while that commit is still main HEAD. Never
delete or move reserved tags: a newer commit gets a new patch, so failed
publications can leave gaps. Avoid rerunning successful container jobs;
rerun failed jobs only to preserve the original SHA image.

Repository rules must allow the workflow token to create `v*` tags and
GitHub Releases and write the GHCR package.
Run release regression tests with `uv run --locked task test SUITE=release`.
