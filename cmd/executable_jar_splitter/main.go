package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stackb/jvm_image/pkg/jartar"
)

type layerFlags []string

func (f *layerFlags) String() string { return strings.Join(*f, ", ") }
func (f *layerFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	input := flag.String("input", "", "path to input JAR file")
	output := flag.String("output", "", "path to fallback output tar file")
	var outputLayers layerFlags
	flag.Var(&outputLayers, "output_layer", "PREFIX=path.tar (repeatable)")
	flag.Parse()

	if *input == "" || *output == "" {
		fmt.Fprintf(os.Stderr, "both --input and --output are required\n")
		os.Exit(1)
	}

	layers := make([]jartar.Layer, 0, len(outputLayers))
	for _, ol := range outputLayers {
		prefix, path, ok := strings.Cut(ol, "=")
		if !ok || prefix == "" || path == "" {
			fmt.Fprintf(os.Stderr, "invalid --output_layer value %q: expected PREFIX=path.tar\n", ol)
			os.Exit(1)
		}
		layers = append(layers, jartar.Layer{
			Prefix:     prefix,
			OutputPath: path,
		})
	}

	if err := jartar.Split(*input, *output, layers); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
