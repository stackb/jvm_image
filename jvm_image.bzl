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

def jvm_image(name, binary, layers = [], maven_lock_file = None, **kwargs):
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
        **kwargs: additional arguments passed to the underlying rule
    """
    if ":" in binary:
        pkg, _, target_name = binary.rpartition(":")
        deploy_jar = pkg + ":" + target_name + "_deploy.jar"
    else:
        deploy_jar = binary + "_deploy.jar"

    _jvm_image(
        name = name,
        binary = binary,
        deploy_jar = deploy_jar,
        layers = layers,
        maven_lock_file = maven_lock_file,
        **kwargs
    )

def _jvm_image_impl(ctx):
    deploy_jar = ctx.file.deploy_jar
    outputs = []
    inputs = [deploy_jar]
    args = ctx.actions.args()
    args.add("--input", deploy_jar)

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

        artifact_ids = ctx.attr.binary[MavenDepsInfo].artifacts.to_list()
        for artifact_id in artifact_ids:
            sanitized = _sanitize_artifact_id(artifact_id)
            artifact_out = ctx.actions.declare_file(ctx.label.name + ".maven." + sanitized + ".tar")
            args.add("--artifact", artifact_id + "=" + artifact_out.path)
            outputs.append(artifact_out)

    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = ctx.executable._tool,
        arguments = [args],
        mnemonic = "JvmImage",
        progress_message = "Splitting deploy jar into layers: %s" % ctx.label,
    )

    return [DefaultInfo(files = depset(outputs))]

_jvm_image = rule(
    implementation = _jvm_image_impl,
    attrs = {
        "binary": attr.label(
            mandatory = True,
            aspects = [_maven_deps_aspect],
            doc = "The java_binary or scala_binary target.",
        ),
        "deploy_jar": attr.label(
            mandatory = True,
            allow_single_file = [".jar"],
            doc = "The _deploy.jar implicit output of the binary.",
        ),
        "layers": attr.string_list(
            default = [],
            doc = "Path prefixes for layer splitting. Each prefix gets its own output tar.",
        ),
        "maven_lock_file": attr.label(
            allow_single_file = [".json"],
            doc = "Maven lock file JSON for artifact-based layer splitting.",
        ),
        "_tool": attr.label(
            default = "//cmd/executable_jar_splitter",
            executable = True,
            cfg = "exec",
            doc = "The executable_jar_splitter Go binary.",
        ),
    },
)
