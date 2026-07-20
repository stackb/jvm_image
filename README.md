# jvm_image

[![CI](https://github.com/stackb/jvm_image/actions/workflows/ci.yml/badge.svg)](https://github.com/stackb/jvm_image/actions/workflows/ci.yml)

`jvm_image` provides a Bazel rule that turns a `java_binary` or `scala_binary`
runtime into OCI-compatible tar layers. It is intended to be consumed by image
rules such as [`rules_img`](https://github.com/bazel-contrib/rules_img).

The public API is pre-1.0. Pin clients to a release tag or commit and review
upgrades before changing the pin.

`jvm_jar_layers` keeps every runtime JAR intact under `/app/lib`, preserving
duplicate resources and the normal JVM classpath model. Maven is not required:
without Maven metadata, the rule places the runtime classpath and its classpath
file in one fallback tar. With a `rules_jvm_external` lock file, Maven
dependencies can instead be placed in separate or grouped layers so
application-only changes retain the same dependency-layer digests for
registry- and fleet-wide cache reuse.

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

## Usage

```starlark
load("@jvm_image//:jvm_jar_layers.bzl", "jvm_jar_layers")

jvm_jar_layers(
    name = "server_layers",
    binary = ":server",
)
```

To partition recognized Maven dependencies into stable cacheable layers, pass
the lock file produced by a `rules_jvm_external` `maven.install` repository:

```starlark
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
available through the target's `classpath` output group. The `binary` must
provide a non-empty JVM runtime classpath; analysis fails otherwise.

## Configuration notes

- `maven_lock_file` is optional. When omitted, every runtime JAR goes to the
  fallback tar.
- `max_layers` limits Maven-derived tar layers. It excludes the fallback tar
  and has no effect without `maven_lock_file`. Set it to `0` to disable
  Maven-derived layers.
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

The example in [`example/hello`](example/hello) demonstrates `jvm_jar_layers`
with `rules_img`.
