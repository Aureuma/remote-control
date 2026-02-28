package main

import (
	"os"

	"github.com/Aureuma/remote-control/internal/safarismoke"
)

func main() {
	os.Exit(safarismoke.RunCLI(os.Args[1:]))
}
