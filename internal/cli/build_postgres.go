//go:build postgres
// +build postgres

package cli

import (
	_ "github.com/pulumi/golang-migrate/v4/database/postgres"
)
