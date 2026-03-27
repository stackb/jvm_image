# jar_layerer

Distributes individual JVM dependency JARs across OCI container layer tarballs
for efficient caching, while preserving each JAR intact.

## Why this exists

Bazel's `singlejar` merges all dependency JARs into a single deploy JAR. During
the merge, duplicate resource files (e.g. `reference.conf`,
`META-INF/services/*`) are resolved with last-writer-wins semantics — silently
dropping entries from other JARs. This causes runtime failures when libraries
like Typesafe Config or Java's `ServiceLoader` expect to scan all JARs on the
classpath for their resources.

`jar_layerer` avoids the problem entirely by keeping each dependency JAR as an
individual file in the container. The JVM loads them via a classpath file, giving
identical behavior to `bazel run` (which already uses individual JARs).

## How it works

```
scala_binary
  └─ JavaInfo.transitive_runtime_jars  (individual JARs from Bazel)
       │
       ▼
  jar_layerer
       │
       ├─ guava.tar          ← layer: guava-31.1.jar
       ├─ pekko.tar          ← layer: pekko-actor_2.13-1.0.jar
       ├─ ...                ← one layer per artifact (or grouped)
       └─ fallback.tar       ← unmatched JARs + classpath file
```

Each output tar contains intact `.jar` files under a configurable path prefix
(default `app/lib/`). A classpath file is written both to disk (for Bazel) and
into the fallback tar (so it's present in the container at runtime).

The container entrypoint uses Java's `@file` syntax to read the classpath:

```
java ${JAVA_OPTS} -cp @/app/lib/classpath com.example.Main
```

## Artifact routing

When a Maven lock file is provided, the tool inspects each JAR to determine
which Maven artifact it belongs to:

1. Open the JAR (ZIP) and find the first `.class` entry.
2. Derive the Java package from the class path (e.g.
   `com/google/common/collect/Lists.class` → `com.google.common.collect`).
3. Look up the package in the lock file's `packages` map to find the artifact ID
   (e.g. `com.google.guava:guava`).
4. Route the JAR to the corresponding artifact layer tar.

JARs that don't match any artifact go to the fallback tar.

When the number of artifacts exceeds `max_layers`, the Starlark rule groups
artifacts by Maven group ID prefix (e.g. all `com.google.*` artifacts share one
layer) using progressively shorter prefixes until under the limit.

## JAR naming

Multiple Bazel targets can produce JARs with the same basename (e.g. many
`scala_library` targets all output `scala.jar`). The tool derives unique names
from the full Bazel output path:

- **External maven JARs** (paths containing `/external/`): use the basename
  as-is, since it's already unique (e.g. `processed_guava-31.1.jar`).
- **Internal workspace JARs** (paths containing `/bin/`): flatten the
  package-relative path with underscores. For example:
  `bazel-out/.../bin/trumid/common/aeron/core/scala.jar` becomes
  `trumid_common_aeron_core_scala.jar`.
- **Fallback**: basename with a numeric suffix on collision.

## Container structure

```
/app/lib/
├── classpath                                    # colon-separated list of JAR paths
├── processed_guava-31.1.jar                     # external maven dep (basename)
├── processed_pekko-actor_2.13-1.0.jar           # external maven dep (basename)
├── trumid_common_aeron_core_scala.jar           # internal (package path flattened)
├── trumid_common_aeron_cluster_shared_scala.jar # internal (package path flattened)
└── ...
```

## Starlark integration

The `jvm_jar_layers` rule in `jvm_image_layers.bzl` drives this tool:

```starlark
load("@jvm_image//:jvm_image_layers.bzl", "jvm_jar_layers")

jvm_jar_layers(
    name = "my_layers",
    binary = ":my_server",
    maven_lock_file = "//:maven_install.json",
)
```

The rule:
- Collects `transitive_runtime_jars` from the binary's `JavaInfo` provider.
- Uses `_maven_deps_aspect` to collect Maven artifact IDs from `maven_coordinates=` tags.
- Writes a jar list file and passes it to `jar_layerer`.
- Declares per-artifact (or per-group) output tars.
- Outputs all tars via `DefaultInfo` and the classpath file via `OutputGroupInfo`.

## CLI flags

| Flag | Description |
|------|-------------|
| `--jar_list` | Path to a file listing JAR paths, one per line |
| `--fallback` | Output tar for JARs not matching any artifact layer (required) |
| `--maven_lock_file` | Maven lock file JSON for package-to-artifact resolution |
| `--classpath` | Output path for the classpath file |
| `--app_prefix` | Classpath prefix in the container (default `/app/lib`) |
| `--path_prefix` | Prefix prepended to tar entry paths (default `app/lib/`) |
| `--artifact_layer` | `ARTIFACT_ID=path.tar` — one artifact per layer (repeatable) |
| `--artifact_group_layer` | `ID1,ID2,...=path.tar` — multiple artifacts sharing a layer (repeatable) |

Positional arguments are also accepted as additional JAR paths.

## Comparison with executable_jar_splitter

| | `executable_jar_splitter` | `jar_layerer` |
|---|---|---|
| Input | Single deploy JAR (merged) | Individual dependency JARs |
| Output | Exploded files in tar layers | Intact JARs in tar layers |
| `reference.conf` | Lost (singlejar last-writer-wins) | Preserved (each JAR has its own) |
| `META-INF/services` | Lost (singlejar last-writer-wins) | Preserved |
| Runtime behavior | May differ from `bazel run` | Identical to `bazel run` |
| Entrypoint | `java -cp /app MainClass` | `java -cp @/app/lib/classpath MainClass` |
