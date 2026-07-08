package main

import (
	_ "github.com/pulumi/golang-migrate/v4/database/mysql"
	"github.com/pulumi/golang-migrate/v4/internal/cli"
	_ "github.com/pulumi/golang-migrate/v4/source/file"
)

func main() {
	cli.Main(Version)
}
