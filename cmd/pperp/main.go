// Command pperp is the Phoenix Perpetuals command line: pperp login, then pperp accounts, pperp buy, pperp stream...
// pperp --help lists every command.
package main

import (
	"os"

	"github.com/phoenixperps/pperp/internal/cli"
	"github.com/phoenixperps/pperp/internal/release"
)

var version = "dev" // set at build: -ldflags "-X main.version=..."; go install: the module version

func main() { os.Exit(cli.Main(release.BuildVersion(version))) }
