//go:build sqlcipher
// +build sqlcipher

package cli

import (
	_ "github.com/pulumi/golang-migrate/v4/database/sqlcipher"
)
