package main

import (
	"os"

	"github.com/si/remote-control/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
