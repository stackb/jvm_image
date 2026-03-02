"""Rule for converting a JVM binary's deploy jar into layered tarballs."""

def _sanitize_prefix(prefix):
    """Convert a path prefix to a safe filename component."""
    return prefix.replace("/", "_").strip("_")

def jvm_image(name, binary, layers = [], **kwargs):
    """Creates layered tarballs from a java_binary or scala_binary deploy jar.

    Args:
        name: target name
        binary: label of a java_binary or scala_binary target
        layers: list of path prefix strings; entries matching a prefix go into
            a separate tar layer. Unmatched entries go to the fallback tar.
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
        **kwargs
    )

def _jvm_image_impl(ctx):
    deploy_jar = ctx.file.deploy_jar
    outputs = []
    args = ctx.actions.args()
    args.add("--input", deploy_jar)

    # Fallback output tar (entries not matching any layer prefix).
    fallback = ctx.actions.declare_file(ctx.label.name + ".tar")
    args.add("--output", fallback)
    outputs.append(fallback)

    # Per-layer output tars.
    for prefix in ctx.attr.layers:
        sanitized = _sanitize_prefix(prefix)
        layer_out = ctx.actions.declare_file(ctx.label.name + "." + sanitized + ".tar")
        args.add("--output_layer", prefix + "=" + layer_out.path)
        outputs.append(layer_out)

    ctx.actions.run(
        inputs = [deploy_jar],
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
        "_tool": attr.label(
            default = "//cmd/executable_jar_splitter",
            executable = True,
            cfg = "exec",
            doc = "The executable_jar_splitter Go binary.",
        ),
    },
)
