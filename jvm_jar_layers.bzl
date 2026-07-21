"""Bazel rule for converting a JVM binary's runtime JARs into container layers."""

load("@rules_java//java/common:java_common.bzl", "java_common")
load("@rules_java//java/common:java_info.bzl", "JavaInfo")

_MavenArtifactsInfo = provider(
    doc = "Collects Maven artifact IDs from jvm_import dependencies.",
    fields = {
        "artifacts": "depset of artifact ID strings (group:name)",
    },
)

def _maven_artifacts_aspect_impl(_target, ctx):
    artifacts = []

    # Check tags for maven_coordinates.
    if hasattr(ctx.rule.attr, "tags"):
        for tag in ctx.rule.attr.tags:
            if tag.startswith("maven_coordinates="):
                coord = tag[len("maven_coordinates="):]
                parts = coord.split(":")
                if len(parts) >= 2:
                    artifact_id = parts[0] + ":" + parts[1]
                    artifacts.append(artifact_id)

    # Collect from transitive deps.
    transitive = []
    for attr_name in ("deps", "exports", "runtime_deps"):
        if hasattr(ctx.rule.attr, attr_name):
            for dep in getattr(ctx.rule.attr, attr_name):
                if _MavenArtifactsInfo in dep:
                    transitive.append(dep[_MavenArtifactsInfo].artifacts)

    return [_MavenArtifactsInfo(
        artifacts = depset(direct = artifacts, transitive = transitive),
    )]

_maven_artifacts_aspect = aspect(
    implementation = _maven_artifacts_aspect_impl,
    attr_aspects = ["deps", "exports", "runtime_deps"],
)

def _sanitize_artifact_id(artifact_id):
    """Convert an artifact ID to a safe filename component."""
    return artifact_id.replace(":", "_")

def _validate_options(max_layers, app_prefix, path_prefix):
    """Validate public macro arguments."""
    if max_layers < 0:
        fail("max_layers must be greater than or equal to zero")
    if not app_prefix.startswith("/"):
        fail("app_prefix must be an absolute container path")
    if ".." in app_prefix.split("/") or ":" in app_prefix or "\\" in app_prefix:
        fail("app_prefix must be a safe absolute container path without ':'")
    if " " in app_prefix or "\t" in app_prefix or "\n" in app_prefix or "\r" in app_prefix:
        fail("app_prefix must not contain whitespace")
    if path_prefix.startswith("/") or ".." in path_prefix.split("/"):
        fail("path_prefix must be relative and must not contain '..'")
    if path_prefix and not path_prefix.endswith("/"):
        fail("non-empty path_prefix must end with '/'")

def _runtime_jars(binary):
    """Return the runtime classpath exposed by a JVM binary target."""
    if hasattr(java_common, "JavaRuntimeClasspathInfo") and java_common.JavaRuntimeClasspathInfo in binary:
        return binary[java_common.JavaRuntimeClasspathInfo].runtime_classpath.to_list()
    return binary[JavaInfo].transitive_runtime_jars.to_list()

def _runfiles_archive_path(file, workspace_name):
    """Return a Bazel-compatible runfiles path rooted below app/."""
    short_path = file.short_path
    if short_path.startswith("../"):
        relative_path = short_path[3:]
    elif short_path.startswith("external/"):
        relative_path = short_path[len("external/"):]
    else:
        relative_path = workspace_name + "/" + short_path
    if not relative_path or ".." in relative_path.split("/"):
        fail("data file %s has unsafe runfiles path %r" % (file.path, relative_path))
    return "app/" + relative_path

def _data_files(ctx, runtime_jars):
    """Collect non-JDK, non-classpath runfiles from the binary and data attr."""
    binary_info = ctx.attr.binary[DefaultInfo]
    transitive = [binary_info.data_runfiles.files]
    for target in ctx.attr.data:
        info = target[DefaultInfo]
        transitive.append(info.files)
        transitive.append(info.default_runfiles.files)
        transitive.append(info.data_runfiles.files)

    excluded = {
        file.path: True
        for file in runtime_jars + binary_info.files.to_list() + ctx.files._jdk
    }
    return [
        file
        for file in depset(transitive = transitive).to_list()
        if file.path not in excluded
    ]

def _group_key(artifact_id, depth):
    """Extract a grouping key from an artifact ID at the given depth.

    For artifact "com.google.guava:guava":
      depth=None -> "com.google.guava" (full group ID)
      depth=2    -> "com.google"
      depth=1    -> "com"

    Args:
        artifact_id: string like "com.google.guava:guava"
        depth: number of dot-segments to keep, or None for full group ID
    Returns:
        grouping key string
    """
    group_id = artifact_id.split(":")[0]
    if depth == None:
        return group_id
    parts = group_id.split(".")
    if depth >= len(parts):
        return group_id
    return ".".join(parts[:depth])

def _group_artifacts(artifact_ids, max_groups):
    """Group artifacts by progressively shorter Maven group prefixes until under max_groups.

    Args:
        artifact_ids: list of artifact ID strings
        max_groups: maximum number of groups allowed
    Returns:
        list of (group_name, [artifact_id, ...]) tuples
    """
    if len(artifact_ids) <= max_groups:
        return [(aid, [aid]) for aid in artifact_ids]

    # Start with full group ID, then progressively shorten.
    # depth=None means full group ID, then 2, 1.
    for depth in [None, 3, 2, 1]:
        groups = {}
        group_order = []
        for aid in sorted(artifact_ids):
            key = _group_key(aid, depth)
            if key not in groups:
                groups[key] = []
                group_order.append(key)
            groups[key].append(aid)

        if len(group_order) <= max_groups:
            return [(key, groups[key]) for key in group_order]

    # Final fallback: merge everything into one group.
    if max_groups >= 1:
        return [("all", sorted(artifact_ids))]

    # max_groups is 0: no artifact layers at all.
    return []

# Non-maven layer slots: the data-runfiles tar and the fallback tar.
_EXTRA_LAYER_SLOTS = 2

def jvm_jar_layer_slots(max_layers):
    """Number of `<name>.layer_N` filegroups emitted for a given max_layers."""
    return max_layers + _EXTRA_LAYER_SLOTS

def jvm_jar_layers(
        name,
        binary,
        data = [],
        maven_lock_file = None,
        max_layers = 121,
        layer_strategy = "group_by_prefix",
        app_prefix = "/app/lib",
        path_prefix = "app/lib/",
        **kwargs):
    """Creates layered tarballs containing individual dependency JARs.

    This preserves each dependency JAR intact, avoiding resource merge
    conflicts involving files such as reference.conf and META-INF/services/*.

    The container classpath uses Java's @file syntax to reference a classpath
    file listing all JARs.

    Because the number and names of the layer tars are only known at analysis
    time, each tar is also exposed through a fixed-name `<name>.layer_N`
    filegroup (N in range(jvm_jar_layer_slots(max_layers))) so image rules can
    map each tar to its own image layer. Slots beyond the produced tar count
    are padded with empty tars, which compress to byte-identical, deduplicable
    layer blobs.

    Args:
        name: target name
        binary: label of a java_binary or scala_binary target
        data: optional files or targets whose files and runfiles are added under
            /app using Bazel's runfiles layout. The binary's own non-JAR data
            runfiles are included automatically.
        maven_lock_file: optional label of a rules_jvm_external lock file JSON
            for Maven artifact-based layer grouping. When omitted, all runtime
            JARs are written to the fallback tar.
        max_layers: maximum number of artifact layers (default 121).
        layer_strategy: strategy when artifacts exceed max_layers.
        app_prefix: classpath prefix inside the container (default "/app/lib").
        path_prefix: prefix prepended to tar entry paths (default "app/lib/").
        **kwargs: additional arguments passed to the underlying rule
    """
    _validate_options(max_layers, app_prefix, path_prefix)

    _jvm_jar_layers(
        name = name,
        binary = binary,
        data = data,
        maven_lock_file = maven_lock_file,
        max_layers = max_layers,
        layer_strategy = layer_strategy,
        app_prefix = app_prefix,
        path_prefix = path_prefix,
        **kwargs
    )

    for index in range(jvm_jar_layer_slots(max_layers)):
        native.filegroup(
            name = "%s.layer_%d" % (name, index),
            srcs = [name],
            output_group = "layer_%d" % index,
        )

def _jvm_jar_layers_impl(ctx):
    runtime_jars = _runtime_jars(ctx.attr.binary)
    if not runtime_jars:
        fail("%s exposes no runtime JARs" % ctx.attr.binary.label)

    # Write a file listing all JAR paths for the tool to read.
    jar_list = ctx.actions.declare_file(ctx.label.name + "_jars.txt")
    ctx.actions.write(
        output = jar_list,
        content = "\n".join([jar.path for jar in runtime_jars]),
    )

    tar_outputs = []
    inputs = list(runtime_jars) + [jar_list]
    args = ctx.actions.args()
    args.add("--jar_list", jar_list)
    args.add("--app_prefix", ctx.attr.app_prefix)
    args.add("--path_prefix", ctx.attr.path_prefix)

    data_files = _data_files(ctx, runtime_jars)
    if data_files:
        data_by_destination = {}
        for file in data_files:
            destination = _runfiles_archive_path(file, ctx.workspace_name)
            previous = data_by_destination.get(destination)
            if previous and previous.path != file.path:
                fail("data files %s and %s collide at /%s" % (previous.path, file.path, destination))
            data_by_destination[destination] = file

        data_manifest = ctx.actions.declare_file(ctx.label.name + "_data.json")
        manifest_entries = [
            {
                "destination": destination,
                "source": data_by_destination[destination].path,
            }
            for destination in sorted(data_by_destination.keys())
        ]
        ctx.actions.write(data_manifest, json.encode(manifest_entries))
        inputs.extend(data_by_destination.values())
        inputs.append(data_manifest)
        args.add("--data_manifest", data_manifest)

        data_layer = ctx.actions.declare_file(ctx.label.name + ".data.tar")
        args.add("--data_layer", data_layer)
        tar_outputs.append(data_layer)

    # Classpath file (not a tar — kept separate from tar outputs).
    classpath_file = ctx.actions.declare_file(ctx.label.name + "_classpath")
    args.add("--classpath", classpath_file)

    # Fallback output tar.
    fallback = ctx.actions.declare_file(ctx.label.name + ".tar")
    args.add("--fallback", fallback)
    tar_outputs.append(fallback)

    # Maven artifact layers via aspect.
    if ctx.file.maven_lock_file:
        lock_file = ctx.file.maven_lock_file
        inputs.append(lock_file)
        args.add("--maven_lock_file", lock_file)

        artifact_ids = sorted(ctx.attr.binary[_MavenArtifactsInfo].artifacts.to_list())
        available_slots = ctx.attr.max_layers
        strategy = ctx.attr.layer_strategy

        if len(artifact_ids) <= available_slots:
            for index, artifact_id in enumerate(artifact_ids):
                sanitized = _sanitize_artifact_id(artifact_id)
                artifact_out = ctx.actions.declare_file(ctx.label.name + ".maven_%d." % index + sanitized + ".tar")
                args.add("--artifact_layer", artifact_id + "=" + artifact_out.path)
                tar_outputs.append(artifact_out)
        elif strategy == "truncate":
            for index, artifact_id in enumerate(artifact_ids[:available_slots]):
                sanitized = _sanitize_artifact_id(artifact_id)
                artifact_out = ctx.actions.declare_file(ctx.label.name + ".maven_%d." % index + sanitized + ".tar")
                args.add("--artifact_layer", artifact_id + "=" + artifact_out.path)
                tar_outputs.append(artifact_out)
        elif strategy == "group_by_prefix":
            groups = _group_artifacts(artifact_ids, available_slots)
            for index, (group_name, group_ids) in enumerate(groups):
                sanitized = _sanitize_artifact_id(group_name)
                group_out = ctx.actions.declare_file(ctx.label.name + ".maven_%d." % index + sanitized + ".tar")
                if len(group_ids) == 1:
                    args.add("--artifact_layer", group_ids[0] + "=" + group_out.path)
                else:
                    args.add("--artifact_group_layer", ",".join(group_ids) + "=" + group_out.path)
                tar_outputs.append(group_out)

    # Pad remaining slots with empty tars so every layer_N output group (and
    # its filegroup) yields exactly one tar file — image rules typically reject
    # labels that produce no tar.
    for index in range(len(tar_outputs), jvm_jar_layer_slots(ctx.attr.max_layers)):
        pad = ctx.actions.declare_file(ctx.label.name + ".pad_%d.tar" % index)
        args.add("--pad_layer", pad)
        tar_outputs.append(pad)

    ctx.actions.run(
        inputs = inputs,
        outputs = tar_outputs + [classpath_file],
        executable = ctx.executable._tool,
        arguments = [args],
        mnemonic = "JvmJarLayers",
        progress_message = "Layering JARs: %s" % ctx.label,
    )

    # DefaultInfo only includes tar files — the classpath file is a plain text
    # file and must not be passed to container_image's tars attribute.
    output_groups = {"classpath": depset([classpath_file])}
    for index in range(jvm_jar_layer_slots(ctx.attr.max_layers)):
        files = [tar_outputs[index]] if index < len(tar_outputs) else []
        output_groups["layer_%d" % index] = depset(files)

    return [
        DefaultInfo(files = depset(tar_outputs)),
        OutputGroupInfo(**output_groups),
    ]

_jvm_jar_layers = rule(
    implementation = _jvm_jar_layers_impl,
    attrs = {
        "binary": attr.label(
            mandatory = True,
            aspects = [_maven_artifacts_aspect],
            doc = "The java_binary or scala_binary target.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Additional files and runfiles to place under /app using Bazel's runfiles layout.",
        ),
        "maven_lock_file": attr.label(
            allow_single_file = [".json"],
            doc = "Maven lock file JSON for artifact-based layer grouping.",
        ),
        "max_layers": attr.int(
            default = 121,
            doc = "Maximum number of artifact layers.",
        ),
        "layer_strategy": attr.string(
            default = "group_by_prefix",
            values = ["truncate", "group_by_prefix"],
            doc = "Strategy when artifacts exceed max_layers.",
        ),
        "app_prefix": attr.string(
            default = "/app/lib",
            doc = "Classpath prefix inside the container.",
        ),
        "path_prefix": attr.string(
            default = "app/lib/",
            doc = "Path prefix prepended to tar entry names.",
        ),
        "_tool": attr.label(
            default = "//cmd/jar_layerer",
            executable = True,
            cfg = "exec",
            doc = "The jar_layerer Go binary.",
        ),
        "_jdk": attr.label(
            default = "@bazel_tools//tools/jdk:current_java_runtime",
            providers = [java_common.JavaRuntimeInfo],
        ),
    },
)
