package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

// slackDashboardInsights defines the insights created by `cc-connect dashboard setup`.
// Each query filters for platform_name='slack' and groups by channel or user.
var slackDashboardInsights = []struct {
	Name  string
	Viz   string // "bar", "line", or "table"
	Query string
}{
	{
		Name: "Turns by Slack channel (last 30d)",
		Viz:  "bar",
		Query: `SELECT
  coalesce(properties.chat_name, properties.chat_id, 'unknown') AS channel,
  count() AS turns
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY channel
ORDER BY turns DESC
LIMIT 25`,
	},
	{
		Name: "Turns by Slack user (last 30d)",
		Viz:  "bar",
		Query: `SELECT
  coalesce(properties.sender_user_name, properties.sender_user_id, 'unknown') AS user,
  count() AS turns
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY user
ORDER BY turns DESC
LIMIT 25`,
	},
	{
		Name: "Tokens by Slack channel (last 30d)",
		Viz:  "bar",
		Query: `SELECT
  coalesce(properties.chat_name, properties.chat_id, 'unknown') AS channel,
  sum(toIntOrZero(toString(properties.input_tokens))) AS input_tokens,
  sum(toIntOrZero(toString(properties.output_tokens))) AS output_tokens,
  sum(toIntOrZero(toString(properties.cache_read_tokens))) AS cache_read
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY channel
ORDER BY input_tokens + output_tokens DESC
LIMIT 25`,
	},
	{
		Name: "Tokens by Slack user (last 30d)",
		Viz:  "bar",
		Query: `SELECT
  coalesce(properties.sender_user_name, properties.sender_user_id, 'unknown') AS user,
  sum(toIntOrZero(toString(properties.input_tokens))) AS input_tokens,
  sum(toIntOrZero(toString(properties.output_tokens))) AS output_tokens,
  sum(toIntOrZero(toString(properties.cache_read_tokens))) AS cache_read
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY user
ORDER BY input_tokens + output_tokens DESC
LIMIT 25`,
	},
	{
		Name: "Daily turns by Slack channel (last 30d)",
		Viz:  "line",
		Query: `SELECT
  toDate(timestamp) AS day,
  coalesce(properties.chat_name, properties.chat_id, 'unknown') AS channel,
  count() AS turns
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY day, channel
ORDER BY day, turns DESC`,
	},
	{
		Name: "Slack usage detail (last 30d)",
		Viz:  "table",
		Query: `SELECT
  coalesce(properties.chat_name, properties.chat_id, 'unknown') AS channel,
  coalesce(properties.sender_user_name, properties.sender_user_id, 'unknown') AS user,
  count() AS turns,
  sum(toIntOrZero(toString(properties.input_tokens))) AS input_tokens,
  sum(toIntOrZero(toString(properties.output_tokens))) AS output_tokens,
  round(avg(toFloatOrZero(toString(properties.turn_duration_ms))) / 1000, 1) AS avg_secs,
  countIf(properties.error_status = 'true') AS errors
FROM events
WHERE event = 'turn_complete'
  AND properties.platform_name = 'slack'
  AND timestamp > now() - interval 30 day
GROUP BY channel, user
ORDER BY turns DESC
LIMIT 100`,
	},
}

func runDashboard(args []string) {
	if len(args) == 0 {
		printDashboardHelp()
		os.Exit(1)
	}
	switch args[0] {
	case "setup":
		runDashboardSetup(args[1:])
	case "-h", "--help", "help":
		printDashboardHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown dashboard subcommand: %s\n", args[0])
		printDashboardHelp()
		os.Exit(1)
	}
}

func printDashboardHelp() {
	fmt.Fprint(os.Stderr, `Usage:
  cc-connect dashboard setup    Create PostHog dashboard for Slack usage

Reads PostHog credentials from the [telemetry] section of config.toml
(personal_api_key and project_id are required).
`)
}

func runDashboardSetup(args []string) {
	fs := flag.NewFlagSet("dashboard setup", flag.ExitOnError)
	name := fs.String("name", "cc-connect Slack Usage", "dashboard name")
	description := fs.String("description", "Turns and tokens broken down by Slack channel and user.", "dashboard description")
	fs.Parse(args)

	cfg, err := config.Load(resolveConfigPath(""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Telemetry.PersonalAPIKey == "" || cfg.Telemetry.ProjectID == "" {
		fmt.Fprintln(os.Stderr, "telemetry.personal_api_key and telemetry.project_id are required in config.toml.")
		fmt.Fprintln(os.Stderr, "Create a personal API key at https://eu.posthog.com/settings/user-api-keys")
		os.Exit(1)
	}

	client := core.NewPostHogQueryClient(
		cfg.Telemetry.PersonalAPIKey,
		cfg.Telemetry.ProjectID,
		cfg.Telemetry.QueryBaseURL,
	)

	fmt.Printf("Creating dashboard %q in project %s...\n", *name, cfg.Telemetry.ProjectID)
	dashboardID, err := client.CreateDashboard(*name, *description)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create dashboard: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  dashboard id: %d\n", dashboardID)

	var failed []string
	for _, in := range slackDashboardInsights {
		fmt.Printf("  + %s\n", in.Name)
		if err := client.CreateHogQLInsight(dashboardID, in.Name, in.Query, in.Viz); err != nil {
			fmt.Fprintf(os.Stderr, "    failed: %v\n", err)
			failed = append(failed, in.Name)
		}
	}

	fmt.Printf("\nDashboard: %s\n", client.DashboardURL(dashboardID))
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d insight(s) failed: %s\n", len(failed), strings.Join(failed, ", "))
		os.Exit(1)
	}
}
