"""Rule for converting a JVM binary's deploy jar into layered tarballs."""

MavenDepsInfo = provider(
    doc = "Collects maven artifact IDs from jvm_import dependencies.",
    fields = {
        "artifacts": "depset of artifact ID strings (group:name)",
    },
)

def _maven_deps_aspect_impl(target, ctx):
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
                if MavenDepsInfo in dep:
                    transitive.append(dep[MavenDepsInfo].artifacts)

    return [MavenDepsInfo(
        artifacts = depset(direct = artifacts, transitive = transitive),
    )]

_maven_deps_aspect = aspect(
    implementation = _maven_deps_aspect_impl,
    attr_aspects = ["deps", "exports", "runtime_deps"],
)

def _sanitize_prefix(prefix):
    """Convert a path prefix to a safe filename component."""
    return prefix.replace("/", "_").strip("_")

def _sanitize_artifact_id(artifact_id):
    """Convert an artifact ID to a safe filename component."""
    return artifact_id.replace(":", "_")

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

def jvm_image_layers(
        name,
        binary,
        layers = [],
        maven_lock_file = None,
        max_layers = 121,
        layer_strategy = "group_by_prefix",
        app_prefix = "/app",
        path_prefix = "app/",
        **kwargs):
    """Creates layered tarballs from a java_binary or scala_binary deploy jar.

    Args:
        name: target name
        binary: label of a java_binary or scala_binary target
        layers: list of path prefix strings; entries matching a prefix go into
            a separate tar layer. Unmatched entries go to the fallback tar.
        maven_lock_file: optional label of a maven lock file JSON. When set,
            the aspect collects maven artifact IDs from deps and the tool
            creates per-artifact tar layers using package prefixes from the
            lock file.
        max_layers: maximum number of artifact layers to generate (default 121).
            Does not count explicit layers or the fallback tar.
        layer_strategy: strategy when artifacts exceed max_layers.
            "truncate": keep first N artifacts alphabetically, rest go to fallback.
            "group_by_prefix": group artifacts by Maven group ID prefix (default).
        app_prefix: classpath prefix inside the container (default "/app").
        path_prefix: prefix prepended to tar entry paths (default "app/").
        **kwargs: additional arguments passed to the underlying rule
    """
    if ":" in binary:
        pkg, _, target_name = binary.rpartition(":")
        deploy_jar = pkg + ":" + target_name + "_deploy.jar"
    else:
        deploy_jar = binary + "_deploy.jar"

    _jvm_image_layers(
        name = name,
        binary = binary,
        deploy_jar = deploy_jar,
        layers = layers,
        maven_lock_file = maven_lock_file,
        max_layers = max_layers,
        layer_strategy = layer_strategy,
        app_prefix = app_prefix,
        path_prefix = path_prefix,
        **kwargs
    )

def _jvm_image_layers_impl(ctx):
    deploy_jar = ctx.file.deploy_jar
    outputs = []
    inputs = [deploy_jar]
    args = ctx.actions.args()
    args.add("--input", deploy_jar)

    # Entrypoint shell script.
    entrypoint = ctx.actions.declare_file(ctx.label.name + "_entrypoint.sh")
    args.add("--entrypoint", entrypoint)
    args.add("--app_prefix", ctx.attr.app_prefix)
    args.add("--path_prefix", ctx.attr.path_prefix)

    # Fallback output tar (entries not matching any layer or artifact prefix).
    fallback = ctx.actions.declare_file(ctx.label.name + ".tar")
    args.add("--output", fallback)
    outputs.append(fallback)

    # Per-layer output tars (explicit prefix layers).
    for prefix in ctx.attr.layers:
        sanitized = _sanitize_prefix(prefix)
        layer_out = ctx.actions.declare_file(ctx.label.name + "." + sanitized + ".tar")
        args.add("--output_layer", prefix + "=" + layer_out.path)
        outputs.append(layer_out)

    # Maven artifact layers via aspect.
    if ctx.file.maven_lock_file:
        lock_file = ctx.file.maven_lock_file
        inputs.append(lock_file)
        args.add("--maven_lock_file", lock_file)

        artifact_ids = sorted(ctx.attr.binary[MavenDepsInfo].artifacts.to_list())
        available_slots = ctx.attr.max_layers - len(ctx.attr.layers)
        strategy = ctx.attr.layer_strategy

        if len(artifact_ids) <= available_slots:
            # Under the limit: one layer per artifact.
            for artifact_id in artifact_ids:
                sanitized = _sanitize_artifact_id(artifact_id)
                artifact_out = ctx.actions.declare_file(ctx.label.name + ".maven." + sanitized + ".tar")
                args.add("--artifact", artifact_id + "=" + artifact_out.path)
                outputs.append(artifact_out)
        elif strategy == "truncate":
            # Truncate: first N artifacts get layers, rest fall to fallback.
            for artifact_id in artifact_ids[:available_slots]:
                sanitized = _sanitize_artifact_id(artifact_id)
                artifact_out = ctx.actions.declare_file(ctx.label.name + ".maven." + sanitized + ".tar")
                args.add("--artifact", artifact_id + "=" + artifact_out.path)
                outputs.append(artifact_out)
        elif strategy == "group_by_prefix":
            # Group by Maven group prefix.
            groups = _group_artifacts(artifact_ids, available_slots)
            for group_name, group_ids in groups:
                sanitized = _sanitize_artifact_id(group_name)
                group_out = ctx.actions.declare_file(ctx.label.name + ".maven." + sanitized + ".tar")
                if len(group_ids) == 1:
                    args.add("--artifact", group_ids[0] + "=" + group_out.path)
                else:
                    args.add("--artifact_group", ",".join(group_ids) + "=" + group_out.path)
                outputs.append(group_out)

    ctx.actions.run(
        inputs = inputs,
        outputs = outputs + [entrypoint],
        executable = ctx.executable._tool,
        arguments = [args],
        mnemonic = "JvmImageLayers",
        progress_message = "Splitting deploy jar into layers: %s" % ctx.label,
    )

    return [
        DefaultInfo(files = depset(outputs)),
        OutputGroupInfo(
            entrypoint = depset([entrypoint]),
        ),
    ]

_jvm_image_layers = rule(
    implementation = _jvm_image_layers_impl,
    attrs = {
        "binary": attr.label(
            mandatory = True,
            aspects = [_maven_deps_aspect],
            doc = "The java_binary or scala_binary target.",
        ),
        "deploy_jar": attr.label(
            mandatory = True,
            allow_single_file = [".jar"],
            doc = "The _deploy.jar implicit output of the java_ or scala_binary.",
        ),
        "layers": attr.string_list(
            default = [],
            doc = "Path prefixes for layer splitting. Each prefix gets its own output tar.",
        ),
        "maven_lock_file": attr.label(
            allow_single_file = [".json"],
            doc = "Maven lock file JSON for artifact-based layer splitting.",
        ),
        "max_layers": attr.int(
            default = 121,
            doc = "Maximum number of artifact layers. Does not count explicit layers or fallback.",
        ),
        "layer_strategy": attr.string(
            default = "group_by_prefix",
            values = ["truncate", "group_by_prefix"],
            doc = "Strategy when artifacts exceed max_layers: 'truncate' or 'group_by_prefix'.",
        ),
        "app_prefix": attr.string(
            doc = "Classpath prefix inside the container.",
            mandatory = True,
        ),
        "path_prefix": attr.string(
            default = "app/",
            doc = "Path prefix prepended to tar entry names.",
        ),
        "_tool": attr.label(
            default = "//cmd/executable_jar_splitter",
            executable = True,
            cfg = "exec",
            doc = "The executable_jar_splitter Go binary.",
        ),
    },
)
