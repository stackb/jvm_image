# Releasing

Releases are created from semantic-version tags such as `v0.1.0`. The release
workflow creates an attested source archive, opens a Bazel Central Registry
(BCR) pull request, and publishes the draft GitHub release after the BCR publish
job succeeds.

## One-time setup

1. Choose and add the repository license before the first public release.
2. Keep the `stackb/bazel-central-registry` fork synchronized with
   `bazelbuild/bazel-central-registry`.
3. Add an Actions secret named `BCR_PUBLISH_TOKEN`. Use a classic personal
   access token belonging to the account that can push to the fork and open a
   pull request against the upstream BCR. It needs `repo` and `workflow` scopes.
4. Review `.bcr/metadata.template.json`, especially the maintainer email, before
   the first publication.

## Cut a release

1. Ensure CI is green and the checkout is clean.
2. Confirm the release notes and public API are ready.
3. Create and push the tag:

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

The tag starts `.github/workflows/release.yml`. If BCR publication needs to be
retried without recreating the GitHub release, run the **Publish to BCR**
workflow manually and supply the existing tag.

The BCR pull request is intentionally opened as a draft. The token owner must
mark it ready for review, which serves as the maintainer approval recognized by
the BCR.

The release preparation step rejects malformed tags and refuses to publish
until a `LICENSE`, `LICENSE.txt`, or `LICENSE.md` file exists.
