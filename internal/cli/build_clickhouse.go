//go:build clickhouse
// +build clickhouse

package cli

import (
	_ "github.com/ClickHouse/clickhouse-go"
	_ "github.com/pulumi/golang-migrate/v4/database/clickhouse"
)
