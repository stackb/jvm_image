package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stackb/jvm_image/pkg/jartar"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ", ") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	input := flag.String("input", "", "path to input JAR file")
	output := flag.String("output", "", "path to fallback output tar file")
	mavenLockFile := flag.String("maven_lock_file", "", "path to maven lock file JSON")
	entrypoint := flag.String("entrypoint", "", "path to write entrypoint shell script")
	appPrefix := flag.String("app_prefix", "/app", "classpath prefix in the container")
	pathPrefix := flag.String("path_prefix", "", "prefix prepended to tar entry paths")
	var outputLayers repeatedFlag
	flag.Var(&outputLayers, "output_layer", "PREFIX=path.tar (repeatable)")
	var artifacts repeatedFlag
	flag.Var(&artifacts, "artifact", "ARTIFACT_ID=path.tar (repeatable)")
	var artifactGroups repeatedFlag
	flag.Var(&artifactGroups, "artifact_group", "ID1,ID2,...=path.tar (repeatable)")
	flag.Parse()

	if *input == "" || *output == "" {
		fmt.Fprintf(os.Stderr, "both --input and --output are required\n")
		os.Exit(1)
	}

	opts := jartar.SplitOptions{
		InputPath:         *input,
		FallbackPath:      *output,
		MavenLockFilePath: *mavenLockFile,
		EntrypointPath:    *entrypoint,
		AppPrefix:         *appPrefix,
		PathPrefix:        *pathPrefix,
	}

	for _, ol := range outputLayers {
		prefix, path, ok := strings.Cut(ol, "=")
		if !ok || prefix == "" || path == "" {
			fmt.Fprintf(os.Stderr, "invalid --output_layer value %q: expected PREFIX=path.tar\n", ol)
			os.Exit(1)
		}
		opts.Layers = append(opts.Layers, jartar.Layer{
			Prefix:     prefix,
			OutputPath: path,
		})
	}

	for _, a := range artifacts {
		id, path, ok := strings.Cut(a, "=")
		if !ok || id == "" || path == "" {
			fmt.Fprintf(os.Stderr, "invalid --artifact value %q: expected ARTIFACT_ID=path.tar\n", a)
			os.Exit(1)
		}
		opts.Artifacts = append(opts.Artifacts, jartar.Artifact{
			ID:         id,
			OutputPath: path,
		})
	}

	// --artifact_group=ID1,ID2,...=path.tar
	// Multiple artifact IDs sharing the same output tar.
	for _, ag := range artifactGroups {
		ids, path, ok := strings.Cut(ag, "=")
		if !ok || ids == "" || path == "" {
			fmt.Fprintf(os.Stderr, "invalid --artifact_group value %q: expected ID1,ID2,...=path.tar\n", ag)
			os.Exit(1)
		}
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			opts.Artifacts = append(opts.Artifacts, jartar.Artifact{
				ID:         id,
				OutputPath: path,
			})
		}
	}

	if _, err := jartar.Split(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
