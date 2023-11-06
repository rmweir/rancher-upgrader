package main

import (
	"github.com/rmweir/rancher-upgrader/cmd"
	"github.com/urfave/cli/v2"
	"log"
	"os"
)

func main() {
	app := &cli.App{
		Name:  "rancher-upgrade",
		Usage: "Upgrade rancher release and inform user on critical changes",
	}

	app.Commands = []*cli.Command{
		cmd.UpgradeCommand(),
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
