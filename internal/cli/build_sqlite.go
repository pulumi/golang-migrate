//go:build sqlite
// +build sqlite

package cli

import (
	_ "github.com/pulumi/golang-migrate/v4/database/sqlite"
)
