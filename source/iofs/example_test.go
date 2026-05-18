//go:build go1.16

package iofs_test

import (
	"embed"
	"log"

	"github.com/pulumi/golang-migrate/v4"
	_ "github.com/pulumi/golang-migrate/v4/database/mysql"
	"github.com/pulumi/golang-migrate/v4/source/iofs"
)

//go:embed testdata/migrations/*.sql
var fs embed.FS

func Example() {
	d, err := iofs.New(fs, "testdata/migrations")
	if err != nil {
		log.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", d, "mysql://user:password@tcp(localhost:3306)/dbname")
	if err != nil {
		log.Fatal(err)
	}
	err = m.Up()
	if err != nil {
		// ...
	}
	// ...
}
