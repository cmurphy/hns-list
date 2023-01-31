package main

import (
	"fmt"
	"os"

	"github.com/cmurphy/hns-list/cli/pkg/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
