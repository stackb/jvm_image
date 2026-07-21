package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stackb/jvm_image/pkg/jarlayer"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ", ") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	fallback := flag.String("fallback", "", "path to fallback output tar for unmatched JARs")
	lockFile := flag.String("maven_lock_file", "", "path to maven lock file JSON")
	classpath := flag.String("classpath", "", "path to write classpath file")
	appPrefix := flag.String("app_prefix", "/app/lib", "classpath prefix in the container")
	pathPrefix := flag.String("path_prefix", "app/lib/", "prefix prepended to tar entry paths")
	jarList := flag.String("jar_list", "", "path to file listing JAR paths, one per line")
	dataManifest := flag.String("data_manifest", "", "path to a JSON data-file manifest")
	dataLayer := flag.String("data_layer", "", "path to the data runfiles output tar")

	var artifactLayers repeatedFlag
	flag.Var(&artifactLayers, "artifact_layer", "ARTIFACT_ID=path.tar (repeatable)")
	var artifactGroupLayers repeatedFlag
	flag.Var(&artifactGroupLayers, "artifact_group_layer", "ID1,ID2,...=path.tar (repeatable)")
	var padLayers repeatedFlag
	flag.Var(&padLayers, "pad_layer", "path to write an empty layer tar (repeatable)")
	var ensureDirs repeatedFlag
	flag.Var(&ensureDirs, "ensure_dir", "directory entry to always create in the fallback tar (repeatable)")

	flag.Parse()

	if *fallback == "" {
		fmt.Fprintf(os.Stderr, "--fallback is required\n")
		os.Exit(1)
	}

	opts := jarlayer.LayerOptions{
		FallbackPath:  *fallback,
		LockFilePath:  *lockFile,
		ClasspathPath: *classpath,
		AppPrefix:     *appPrefix,
		PathPrefix:    *pathPrefix,
		EnsureDirs:    ensureDirs,
	}

	// Read JAR list from file.
	if *jarList != "" {
		data, err := os.ReadFile(*jarList)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading jar list: %v\n", err)
			os.Exit(1)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				opts.JarPaths = append(opts.JarPaths, line)
			}
		}
	}

	// Also accept JARs as positional args.
	opts.JarPaths = append(opts.JarPaths, flag.Args()...)

	// Parse artifact layers: ARTIFACT_ID=path.tar
	for _, al := range artifactLayers {
		id, outputPath, ok := strings.Cut(al, "=")
		if !ok || id == "" || outputPath == "" {
			fmt.Fprintf(os.Stderr, "invalid --artifact_layer %q: expected ARTIFACT_ID=path.tar\n", al)
			os.Exit(1)
		}
		opts.ArtifactLayers = append(opts.ArtifactLayers, jarlayer.ArtifactLayer{
			IDs:        []string{id},
			OutputPath: outputPath,
		})
	}

	// Parse grouped artifact layers: ID1,ID2,...=path.tar
	for _, agl := range artifactGroupLayers {
		idsStr, outputPath, ok := strings.Cut(agl, "=")
		if !ok || idsStr == "" || outputPath == "" {
			fmt.Fprintf(os.Stderr, "invalid --artifact_group_layer %q: expected ID1,ID2,...=path.tar\n", agl)
			os.Exit(1)
		}
		var ids []string
		for _, id := range strings.Split(idsStr, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
		opts.ArtifactLayers = append(opts.ArtifactLayers, jarlayer.ArtifactLayer{
			IDs:        ids,
			OutputPath: outputPath,
		})
	}

	if err := jarlayer.LayerJars(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if (*dataManifest == "") != (*dataLayer == "") {
		fmt.Fprintln(os.Stderr, "--data_manifest and --data_layer must be set together")
		os.Exit(1)
	}
	if *dataManifest != "" {
		data, err := os.ReadFile(*dataManifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading data manifest: %v\n", err)
			os.Exit(1)
		}
		var files []jarlayer.DataFile
		if err := json.Unmarshal(data, &files); err != nil {
			fmt.Fprintf(os.Stderr, "parsing data manifest: %v\n", err)
			os.Exit(1)
		}
		if err := jarlayer.LayerData(*dataLayer, files); err != nil {
			fmt.Fprintf(os.Stderr, "layering data: %v\n", err)
			os.Exit(1)
		}
	}

	for _, path := range padLayers {
		if err := jarlayer.WriteEmptyTar(path); err != nil {
			fmt.Fprintf(os.Stderr, "writing pad layer: %v\n", err)
			os.Exit(1)
		}
	}
}
