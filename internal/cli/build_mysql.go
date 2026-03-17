//go:build mysql
// +build mysql

package cli

import (
	_ "github.com/pulumi/golang-migrate/v4/database/mysql"
)
