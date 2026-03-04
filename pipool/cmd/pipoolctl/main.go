package main

import (
	"os"

	"github.com/dakota/pipool/internal/ctl"
)

func main() {
	ctl.RunCLI(os.Args[1:])
}
