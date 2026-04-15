package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func runUsage(args []string) {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	days := fs.Int("days", 7, "number of days to look back")
	project := fs.String("project", "", "filter by project name")
	format := fs.String("format", "table", "output format: table or json")
	fs.Parse(args)

	configPath := resolveConfigPath("")
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	if !cfg.Telemetry.TelemetryEnabled() {
		fmt.Fprintln(os.Stderr, "Telemetry is disabled. Remove [telemetry] disabled=true from config.toml.")
		os.Exit(1)
	}
	if cfg.Telemetry.PersonalAPIKey == "" || cfg.Telemetry.ProjectID == "" {
		fmt.Fprintln(os.Stderr, "telemetry.personal_api_key and telemetry.project_id are required for usage queries.")
		os.Exit(1)
	}

	client := core.NewPostHogQueryClient(
		cfg.Telemetry.PersonalAPIKey,
		cfg.Telemetry.ProjectID,
		cfg.Telemetry.QueryBaseURL,
	)

	where := fmt.Sprintf("event = 'turn_complete' AND timestamp > now() - interval %d day", *days)
	if *project != "" {
		where += fmt.Sprintf(" AND properties.project_name = '%s'", *project)
	}

	query := fmt.Sprintf(`SELECT
  properties.project_name AS project,
  properties.platform_name AS platform,
  properties.agent_type AS agent,
  count() AS turns,
  sum(toInt64OrZero(toString(properties.input_tokens))) AS input_tokens,
  sum(toInt64OrZero(toString(properties.output_tokens))) AS output_tokens,
  sum(toInt64OrZero(toString(properties.cache_read_tokens))) AS cache_read,
  sum(toInt64OrZero(toString(properties.cache_creation_tokens))) AS cache_write,
  round(avg(toFloat64OrZero(toString(properties.turn_duration_ms))) / 1000, 1) AS avg_secs,
  countIf(properties.error_status = 'true') AS errors
FROM events
WHERE %s
GROUP BY project, platform, agent
ORDER BY turns DESC`, where)

	result, err := client.Query(query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Query failed: %v\n", err)
		os.Exit(1)
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]interface{}{
			"columns": result.Columns,
			"rows":    result.Results,
		})
		return
	}

	if len(result.Results) == 0 {
		fmt.Printf("No usage data found for the last %d days.\n", *days)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROJECT\tPLATFORM\tAGENT\tTURNS\tINPUT\tOUTPUT\tCACHE_READ\tCACHE_WRITE\tAVG_SECS\tERRORS\n")
	for _, row := range result.Results {
		for i, col := range row {
			if i > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprintf(w, "%v", col)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
}
