//go:build pgx
// +build pgx

package cli

import (
	_ "github.com/pulumi/golang-migrate/v4/database/pgx"
)
