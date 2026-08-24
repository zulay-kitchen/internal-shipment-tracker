// Command package-level-detail prompts for (or takes as arguments) a date
// range and writes a CSV with one row per package (box) on every Goflow
// order whose shipment.shipped_at falls in that range:
//
//	tracking number, order number, carrier, shipping method, ship date,
//	weight amount, weight measure, length, width, height, dimensions
//	measure, destination ZIP, destination state, shipping charge
//
// Orders with no shipment, or a shipment with no boxes, have no
// package-level detail to report and are skipped. A box missing a
// particular field (Goflow doesn't always have weight/dimensions recorded)
// just leaves that column blank rather than failing the row.
//
// After the CSV is written, a per-carrier tally is printed to stdout:
// how many boxes were seen for that carrier, and how many of those were
// missing weight and/or dimensions - a quick signal for how complete that
// carrier's package-level data actually is.
//
// Required environment variables:
//
//	GOFLOW_SUBDOMAIN   the subdomain you use to access Goflow
//	                   (e.g. "companyname" for companyname.goflow.com)
//	GOFLOW_API_TOKEN   a Goflow API token (Settings -> API Tokens)
//
// Any of the above may instead be placed in a ".env" file next to this
// program's source (or its compiled binary), one KEY=VALUE per line. Actual
// environment variables always take precedence over the .env file.
//
// Usage:
//
//	go run . [flags] [date range]
//	package-level-detail [flags] [date range]
//
// The date range is "YYYY-MM-DD to YYYY-MM-DD" (or just "YYYY-MM-DD",
// meaning that date through today), e.g.:
//
//	package-level-detail 2026-01-01 to 2026-01-31
//	package-level-detail 2026-01-01
//
// If omitted, you'll be prompted for it interactively instead.
//
// Flags:
//
//	-dir string   directory to write the CSV to (default ".", created if
//	              it doesn't already exist)
//	-h, -help     show usage and exit
//
// "help" as the first (and only) argument works the same as -h.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"internal-shipment-tracker/internal/env"
	"internal-shipment-tracker/internal/goflow"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	goflowSubdomain string
	goflowToken     string
}

func loadConfig() (config, error) {
	cfg := config{
		goflowSubdomain: os.Getenv("GOFLOW_SUBDOMAIN"),
		goflowToken:     os.Getenv("GOFLOW_API_TOKEN"),
	}
	if cfg.goflowSubdomain == "" || cfg.goflowToken == "" {
		return cfg, fmt.Errorf("GOFLOW_SUBDOMAIN and GOFLOW_API_TOKEN environment variables are required")
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Date range parsing
// ---------------------------------------------------------------------------

// The "to" date is optional - if omitted, parseDateRange fills in today's
// date (UTC) as the end date, so a single date means "from that date
// through now."
var dateRangeRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\s+to\s+(\d{4}-\d{2}-\d{2}))?$`)

// parseDateRange parses a "YYYY-MM-DD to YYYY-MM-DD" or single "YYYY-MM-DD"
// string and returns the start and end dates (inclusive), both at midnight
// UTC.
func parseDateRange(s string) (start, end time.Time, err error) {
	s = strings.TrimSpace(s)
	m := dateRangeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf(`date range must look like "2026-01-01 to 2026-01-31" or just "2026-01-01"`)
	}

	start, err = time.ParseInLocation("2006-01-02", m[1], time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start date: %w", err)
	}

	if m[2] == "" {
		now := time.Now().UTC()
		end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		end, err = time.ParseInLocation("2006-01-02", m[2], time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid end date: %w", err)
		}
	}

	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end date must not be before start date")
	}
	return start, end, nil
}

// promptDateRange asks the user for a date range on the given reader/writer,
// in the same format parseDateRange accepts.
func promptDateRange(in io.Reader, out io.Writer) (start, end time.Time, err error) {
	reader := bufio.NewReader(in)
	fmt.Fprint(out, "Enter date range (YYYY-MM-DD to YYYY-MM-DD, or just YYYY-MM-DD to use today as the end date): ")
	line, readErr := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if readErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("failed to read input: %w", readErr)
		}
		return time.Time{}, time.Time{}, fmt.Errorf("no input given")
	}
	return parseDateRange(line)
}

// ---------------------------------------------------------------------------
// CSV rows
// ---------------------------------------------------------------------------

// csvHeader is the fixed column order for the output CSV.
var csvHeader = []string{
	"tracking number",
	"order number",
	"carrier",
	"shipping method",
	"ship date",
	"weight amount",
	"weight measure",
	"length",
	"width",
	"height",
	"dimensions measure",
	"destination zip",
	"destination state",
	"shipping charge",
}

// formatFloat renders a float the way spreadsheet software expects: no
// trailing zeros, but also no scientific notation for ordinary package
// weights/dimensions/costs.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// boxRow turns a single order+box pair into one CSV row, matching
// csvHeader's column order. Any field Goflow didn't record (weight,
// dimensions, cost, shipping_address, tracking_number, carrier, shipped_at
// are all optional in practice) is left blank rather than failing the row.
func boxRow(o goflow.Order, box goflow.Box) []string {
	trackingNumber := ""
	if box.TrackingNumber != nil {
		trackingNumber = *box.TrackingNumber
	}

	carrier := ""
	if o.Shipment != nil && o.Shipment.Carrier != nil {
		carrier = *o.Shipment.Carrier
	}

	shippingMethod := ""
	if o.Shipment != nil && o.Shipment.ShippingMethod != nil {
		shippingMethod = *o.Shipment.ShippingMethod
	}

	shipDate := ""
	if o.Shipment != nil && o.Shipment.ShippedAt != nil {
		shipDate = o.Shipment.ShippedAt.UTC().Format("2006-01-02")
	}

	weightAmount, weightMeasure := "", ""
	if box.Weight != nil {
		weightAmount = formatFloat(box.Weight.Amount)
		weightMeasure = box.Weight.Measure
	}

	length, width, height, dimensionsMeasure := "", "", "", ""
	if box.Dimensions != nil {
		length = formatFloat(box.Dimensions.Length)
		width = formatFloat(box.Dimensions.Width)
		height = formatFloat(box.Dimensions.Height)
		dimensionsMeasure = box.Dimensions.Measure
	}

	destinationZIP, destinationState := "", ""
	if o.ShippingAddress != nil {
		destinationZIP = o.ShippingAddress.ZIPCode
		destinationState = o.ShippingAddress.State
	}

	shippingCharge := ""
	if box.Cost != nil {
		shippingCharge = formatFloat(box.Cost.Amount)
	}

	return []string{
		trackingNumber,
		o.OrderNumber,
		carrier,
		shippingMethod,
		shipDate,
		weightAmount,
		weightMeasure,
		length,
		width,
		height,
		dimensionsMeasure,
		destinationZIP,
		destinationState,
		shippingCharge,
	}
}

// ---------------------------------------------------------------------------
// Carrier completeness tally
// ---------------------------------------------------------------------------

// carrierTally counts, for one carrier, how many boxes were seen in total
// and how many of those were missing weight and/or dimensions - a quick
// signal for how trustworthy that carrier's package-level data actually is.
type carrierTally struct {
	boxes             int
	missingWeight     int
	missingDimensions int
}

// tallyBox records one box's carrier and whether it was missing weight
// and/or dimensions into tallies, keyed by the exact carrier string boxRow
// would put in the CSV's "carrier" column (including "" for unknown/blank).
func tallyBox(tallies map[string]*carrierTally, carrier string, box goflow.Box) {
	t := tallies[carrier]
	if t == nil {
		t = &carrierTally{}
		tallies[carrier] = t
	}
	t.boxes++
	if box.Weight == nil {
		t.missingWeight++
	}
	if box.Dimensions == nil {
		t.missingDimensions++
	}
}

// printCarrierTally writes a small summary table to out: one row per
// carrier (sorted alphabetically, with unknown/blank carriers grouped under
// "(unknown)"), showing how many boxes were seen and how many were missing
// weight/dimensions.
func printCarrierTally(out io.Writer, tallies map[string]*carrierTally) {
	if len(tallies) == 0 {
		return
	}

	carriers := make([]string, 0, len(tallies))
	for carrier := range tallies {
		carriers = append(carriers, carrier)
	}
	sort.Strings(carriers)

	fmt.Fprintln(out, "\nCarrier tally (package-level data completeness):")
	fmt.Fprintf(out, "  %-20s %8s %15s %19s\n", "carrier", "boxes", "missing weight", "missing dimensions")
	for _, carrier := range carriers {
		label := carrier
		if label == "" {
			label = "(unknown)"
		}
		t := tallies[carrier]
		fmt.Fprintf(out, "  %-20s %8d %15d %19d\n", label, t.boxes, t.missingWeight, t.missingDimensions)
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "package-level-detail")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Pulls Goflow orders shipped in a date range and writes one CSV row per")
	fmt.Fprintln(out, "package (tracking number, order number, carrier, shipping method, ship")
	fmt.Fprintln(out, "date, weight, dimensions, destination ZIP/state, and shipping charge),")
	fmt.Fprintln(out, "then prints a per-carrier tally of boxes missing weight/dimensions.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  package-level-detail [flags] [date range]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, `Date range is "YYYY-MM-DD to YYYY-MM-DD" or just "YYYY-MM-DD" (through`)
	fmt.Fprintln(out, "today). If omitted, you'll be prompted for it interactively.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	flag.CommandLine.SetOutput(out)
	flag.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configuration (environment variables, or a \".env\" file next to this")
	fmt.Fprintln(out, "program):")
	fmt.Fprintln(out, "  GOFLOW_SUBDOMAIN, GOFLOW_API_TOKEN   required")
}

func main() {
	var outDir string
	flag.StringVar(&outDir, "dir", ".", "directory to write the CSV to (created if it doesn't exist)")
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	if flag.Arg(0) == "help" {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Error creating output directory:", err)
		os.Exit(1)
	}

	if err := env.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	// A date range given on the command line (possibly split across
	// several argv entries by the shell, e.g. `2026-01-01 to 2026-01-31`)
	// is joined back into one string and parsed the same way a prompted
	// answer would be. With no arguments, fall back to the interactive
	// prompt.
	var start, end time.Time
	if flag.NArg() > 0 {
		start, end, err = parseDateRange(strings.Join(flag.Args(), " "))
	} else {
		start, end, err = promptDateRange(os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	endExclusive := end.Add(24 * time.Hour)

	goflowClient := goflow.NewClient(cfg.goflowSubdomain, cfg.goflowToken)
	goflowClient.Notice = os.Stdout

	fmt.Printf("Fetching shipped orders from %s to %s...\n", start.Format("2006-01-02"), end.Format("2006-01-02"))
	orders, err := goflowClient.FetchShippedOrders(context.Background(), start, endExclusive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error fetching orders from Goflow:", err)
		os.Exit(1)
	}
	fmt.Printf("Found %d shipped order(s) (before filtering by exact ship date).\n", len(orders))

	var rows [][]string
	tallies := map[string]*carrierTally{}
	for _, o := range orders {
		if o.Shipment == nil || o.Shipment.ShippedAt == nil {
			continue
		}
		shippedAt := o.Shipment.ShippedAt.UTC()
		if shippedAt.Before(start) || !shippedAt.Before(endExclusive) {
			// status_updated_at fell in range (Goflow's own filter), but the
			// actual shipped_at timestamp doesn't - skip it so the report
			// only contains what was actually asked for.
			continue
		}
		carrier := ""
		if o.Shipment.Carrier != nil {
			carrier = *o.Shipment.Carrier
		}
		for _, box := range o.Shipment.Boxes {
			rows = append(rows, boxRow(o, box))
			tallyBox(tallies, carrier, box)
		}
	}

	csvPath := filepath.Join(outDir, fmt.Sprintf("package_level_detail_%s_to_%s.csv", start.Format("2006-01-02"), end.Format("2006-01-02")))
	f, err := os.Create(csvPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating CSV file:", err)
		os.Exit(1)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	_ = w.Write(csvHeader)
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintln(os.Stderr, "Error writing CSV file:", err)
		os.Exit(1)
	}

	fmt.Printf("Wrote %d row(s) to %s\n", len(rows), csvPath)

	printCarrierTally(os.Stdout, tallies)
}
