package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/stackb/jvm_image/pkg/jartar"
)

func main() {
	input := flag.String("input", "", "path to input JAR file")
	output := flag.String("output", "", "path to output tar file")
	flag.Parse()

	if *input == "" || *output == "" {
		fmt.Fprintf(os.Stderr, "both --input and --output are required\n")
		os.Exit(1)
	}

	if err := jartar.Convert(*input, *output); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
