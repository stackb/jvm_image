# jvm_image

[![CI](https://github.com/stackb/jvm_image/actions/workflows/ci.yml/badge.svg)](https://github.com/stackb/jvm_image/actions/workflows/ci.yml)

`jvm_image` provides Bazel rules that turn a `java_binary` or `scala_binary`
runtime into OCI-compatible tar layers. It is intended to be consumed by image
rules such as [`rules_img`](https://github.com/bazel-contrib/rules_img).

The public API is pre-1.0. Pin clients to a release tag or commit and review
upgrades before changing the pin.

## Choose a rule

| Rule               | Layout                                          | Use when                                                                               |
|--------------------|-------------------------------------------------|----------------------------------------------------------------------------------------|
| `jvm_jar_layers`   | Keeps every runtime JAR intact under `/app/lib` | Recommended. Preserves duplicate resources and matches the normal JVM classpath model. |
| `jvm_image_layers` | Explodes one deploy JAR under `/app`            | Use only when the deploy JAR's merged-resource behavior is acceptable.                 |

Both rules always produce a fallback tar for unmatched content. With a
`rules_jvm_external` lock file, Maven dependencies can be placed in separate or
grouped layers so application-only changes reuse dependency layers.

## Install

Until the module is published to a registry, add a pinned override to the
client's `MODULE.bazel`:

```starlark
bazel_dep(name = "jvm_image", version = "0.1.0")

git_override(
    module_name = "jvm_image",
    commit = "<full-commit-sha>",
    remote = "https://github.com/stackb/jvm_image.git",
)
```

Do not point production clients at a moving branch.

## Recommended usage: intact JARs

```starlark
load("@jvm_image//:jvm_image_layers.bzl", "jvm_jar_layers")

jvm_jar_layers(
    name = "server_layers",
    binary = ":server",
    maven_lock_file = "//:maven_install.json",
    max_layers = 32,
)
```

Pass `:server_layers` to the image rule's layer/tar attribute. Configure the
image entrypoint with the binary's main class:

```starlark
entrypoint = [
    "java",
    "-cp",
    "@/app/lib/classpath",
    "com.example.Server",
]
```

The generated `classpath` file is included in the fallback tar. It is also
available through the target's `classpath` output group.

## Deploy-JAR usage

```starlark
load("@jvm_image//:jvm_image_layers.bzl", "jvm_image_layers")

jvm_image_layers(
    name = "server_layers",
    binary = ":server",
    layers = ["com/example/"],
    maven_lock_file = "//:maven_install.json",
    max_layers = 32,
)
```

This rule reads `Main-Class` from `META-INF/MANIFEST.MF` and exposes a generated
launcher through the `entrypoint` output group. The exploded classes are rooted
at `/app` by default.

## Configuration notes

- `max_layers` limits Maven-derived tar layers. It excludes explicit prefix
  layers and the fallback tar. Set it to `0` to disable Maven-derived layers.
- `layer_strategy = "group_by_prefix"` groups Maven coordinates when their
  count exceeds `max_layers`; `"truncate"` sends the excess to the fallback.
- `path_prefix` is a relative archive prefix and must end in `/` when non-empty.
  `app_prefix` is the corresponding absolute path inside the image.
- Artifact routing uses the lock file's `packages` map. Unmatched or ambiguous
  package ownership goes to the fallback instead of selecting a layer based on
  map or ZIP iteration order.
- The intact-JAR classpath format targets Linux containers. Paths containing
  whitespace or `:` are rejected because `:` is the classpath separator.

## Validate locally

```sh
go test ./...
go vet ./...
staticcheck ./...
bazel test //...

cd example/hello
bazel build //:image
```

The example in [`example/hello`](example/hello) demonstrates the exploded
deploy-JAR rule with `rules_img`.

## Release checklist

Before onboarding a client, pin a green commit, choose and add a repository
license, and publish a matching `v0.1.x` tag. A license and hosted release are
intentionally not inferred by this repository.
