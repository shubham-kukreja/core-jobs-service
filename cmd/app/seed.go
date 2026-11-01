package main

import (
	"fmt"
	"io"

	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

func runSeed(args []string, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "validate" {
		return errUsage
	}
	docs, err := seed.Validate(args[1])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%d seed files valid\n", len(docs))
	return err
}
