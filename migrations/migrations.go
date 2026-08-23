package migrations

import _ "embed"

// Migration 是按版本顺序执行的一次数据库结构升级。
type Migration struct {
	Version int
	SQL     string
}

//go:embed 001_init.sql
var initialSQL string

//go:embed 002_review_unit_input.sql
var reviewUnitInputSQL string

//go:embed 003_review_results.sql
var reviewResultsSQL string

//go:embed 004_verified_findings.sql
var verifiedFindingsSQL string

//go:embed 005_reports.sql
var reportsSQL string

//go:embed 006_agent_steps.sql
var agentStepsSQL string

// All 按版本号升序返回全部 migration。
var All = []Migration{
	{Version: 1, SQL: initialSQL},
	{Version: 2, SQL: reviewUnitInputSQL},
	{Version: 3, SQL: reviewResultsSQL},
	{Version: 4, SQL: verifiedFindingsSQL},
	{Version: 5, SQL: reportsSQL},
	{Version: 6, SQL: agentStepsSQL},
}
