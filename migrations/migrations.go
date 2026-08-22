package migrations

import _ "embed"

// SQL contains the initial database schema.
//
//go:embed 001_init.sql
var SQL string
