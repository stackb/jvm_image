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
	var outputLayers repeatedFlag
	flag.Var(&outputLayers, "output_layer", "PREFIX=path.tar (repeatable)")
	var artifacts repeatedFlag
	flag.Var(&artifacts, "artifact", "ARTIFACT_ID=path.tar (repeatable)")
	flag.Parse()

	if *input == "" || *output == "" {
		fmt.Fprintf(os.Stderr, "both --input and --output are required\n")
		os.Exit(1)
	}

	opts := jartar.SplitOptions{
		InputPath:         *input,
		FallbackPath:      *output,
		MavenLockFilePath: *mavenLockFile,
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

	if err := jartar.Split(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
