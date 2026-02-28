package main

import (
	"os"

	"github.com/Aureuma/remote-control/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
