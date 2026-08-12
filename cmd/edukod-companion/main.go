package main

import (
	"os"

	"github.com/edukod-cz/edukod-companion/internal/app"
)

func main() {
	os.Exit(app.New().Run(os.Args[1:]))
}
