"""Rule for converting a JVM binary's deploy jar into a tarball."""

def jvm_image(name, binary, **kwargs):
    """Creates a tarball from a java_binary or scala_binary deploy jar.

    Args:
        name: target name
        binary: label of a java_binary or scala_binary target
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
        **kwargs
    )

def _jvm_image_impl(ctx):
    deploy_jar = ctx.file.deploy_jar
    output = ctx.actions.declare_file(ctx.label.name + ".tar")

    ctx.actions.run(
        inputs = [deploy_jar],
        outputs = [output],
        executable = ctx.executable._tool,
        arguments = [
            "--input",
            deploy_jar.path,
            "--output",
            output.path,
        ],
        mnemonic = "JvmImage",
        progress_message = "Converting deploy jar to tar: %s" % ctx.label,
    )

    return [DefaultInfo(files = depset([output]))]

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
        "_tool": attr.label(
            default = "//cmd/executable_jar_splitter",
            executable = True,
            cfg = "exec",
            doc = "The executable_jar_splitter Go binary.",
        ),
    },
)
