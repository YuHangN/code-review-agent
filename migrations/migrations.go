package migrations

import _ "embed"

// SQL 包含初始数据库 schema。
//
//go:embed 001_init.sql
var SQL string
