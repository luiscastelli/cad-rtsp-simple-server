package main

import (
	"os"

	"github.com/luiscastelli/cad-rtsp-simple-server/internal/core"
)

func main() {
	s, ok := core.New(os.Args[1:])
	if !ok {
		os.Exit(1)
	}
	s.Wait()
}
