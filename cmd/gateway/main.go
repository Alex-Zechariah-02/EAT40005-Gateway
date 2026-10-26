// Command gateway runs the security policy gateway on its own Go HTTP server,
// from one strict native configuration file, and its local operator commands.
package main

import (
	"os"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/gateway"
)

func main() { os.Exit(gateway.Main(os.Args[1:])) }
