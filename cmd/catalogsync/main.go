package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"supplier-catalog-updates/internal/model"
	"supplier-catalog-updates/internal/store"
	"supplier-catalog-updates/internal/supplier"
	"supplier-catalog-updates/internal/syncer"
)

func printUsage() {
	fmt.Printf(`Usage: catalogsync [flags] <command> [arguments]

Commands:
  start                Start a new catalog sync run
  resume [run-id]      Resume an interrupted run (defaults to latest)
  status [run-id]      Show current progress and counters for a run
  report [run-id]      Show status, changes log, and problems
  products             List products in the catalog
                       Flags: -status [active|withdrawn]
  problems             List recorded problems
                       Flags: -run [run-id] -all
  resolve <problem-id> Mark a problem as resolved

Global Flags:
  -db string          Path to SQLite database (default "catalog.db")
  -supplier string    Supplier API base URL (default "http://localhost:8080")
  -retries int        Max retries for transient errors (default 3)
  -backoff duration   Base backoff duration (default 200ms)
  -batch-size int     Number of items per batch (default 20)
  -v                  Enable verbose logging

Examples:
  catalogsync start
  catalogsync resume
  catalogsync status
  catalogsync report
  catalogsync products -status active
  catalogsync problems
  catalogsync resolve <problem-id>
`)
}

func main() {
	var (
		dbPath    = flag.String("db", "catalog.db", "Path to SQLite database")
		supURL    = flag.String("supplier", "http://localhost:8080", "Supplier API base URL")
		retries   = flag.Int("retries", 3, "Max retries for transient errors")
		backoff   = flag.Duration("backoff", 200*time.Millisecond, "Base backoff duration")
		batchSize = flag.Int("batch-size", 20, "Number of items per batch")
		verbose   = flag.Bool("v", false, "Verbose output")
	)
	flag.Usage = printUsage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}

	// Support "catalogsync sync start" or "catalogsync sync resume"
	if args[0] == "sync" {
		if len(args) < 2 {
			printUsage()
			os.Exit(1)
		}
		args = args[1:]
	}

	cmd := args[0]
	cmdArgs := args[1:]

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database %q: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := syncer.RunnerConfig{
		Retries:   *retries,
		Backoff:   *backoff,
		BatchSize: *batchSize,
		Verbose:   *verbose,
	}
	supClient := supplier.NewHTTPClient(*supURL, 15*time.Second)
	runner := syncer.NewRunner(st, supClient, cfg)

	switch cmd {
	case "start":
		handleStart(ctx, runner)
	case "resume":
		runID := ""
		if len(cmdArgs) > 0 {
			runID = cmdArgs[0]
		}
		handleResume(ctx, runner, runID)
	case "status":
		runID := ""
		if len(cmdArgs) > 0 {
			runID = cmdArgs[0]
		}
		handleStatus(ctx, st, runID)
	case "report":
		runID := ""
		if len(cmdArgs) > 0 {
			runID = cmdArgs[0]
		}
		handleReport(ctx, st, runID)
	case "products":
		handleProducts(ctx, st, cmdArgs)
	case "problems":
		handleProblems(ctx, st, cmdArgs)
	case "resolve":
		if len(cmdArgs) < 1 {
			fmt.Fprintln(os.Stderr, "Usage: catalogsync resolve <problem-id>")
			os.Exit(1)
		}
		handleResolve(ctx, st, cmdArgs[0])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func handleStart(ctx context.Context, runner *syncer.Runner) {
	fmt.Println("==> Starting catalog sync run...")
	run, err := runner.Start(ctx)
	displayRunResult(run, err)
}

func handleResume(ctx context.Context, runner *syncer.Runner, runID string) {
	if runID == "" {
		fmt.Println("==> Resuming latest interrupted run...")
	} else {
		fmt.Printf("==> Resuming run %s...\n", runID)
	}
	run, err := runner.Resume(ctx, runID)
	displayRunResult(run, err)
}

func displayRunResult(run *model.UpdateRun, err error) {
	if run != nil {
		printRunSummary(run)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[FAILED/INTERRUPTED] %v\n", err)
		os.Exit(1)
	}

	if run != nil {
		if run.Status == model.RunStatusCompleted {
			fmt.Println("\n✓ Run completed successfully with no problems.")
			os.Exit(0)
		} else if run.Status == model.RunStatusCompletedWithProblems {
			fmt.Println("\n! Run completed with problems requiring attention.")
			os.Exit(0)
		}
	}
	os.Exit(1)
}

func printRunSummary(run *model.UpdateRun) {
	fmt.Println("\n================ RUN SUMMARY ================")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintf(w, "Run ID:\t%s\n", run.ID)
	fmt.Fprintf(w, "Status:\t%s\n", run.Status)
	fmt.Fprintf(w, "Batches Done:\t%d\n", run.BatchesDone)
	fmt.Fprintf(w, "Created:\t%d\n", run.CreatedCount)
	fmt.Fprintf(w, "Updated:\t%d\n", run.UpdatedCount)
	fmt.Fprintf(w, "Withdrawn:\t%d\n", run.WithdrawnCount)
	fmt.Fprintf(w, "Unchanged:\t%d\n", run.UnchangedCount)
	fmt.Fprintf(w, "Stale:\t%d\n", run.StaleCount)
	fmt.Fprintf(w, "Problems:\t%d\n", run.ProblemCount)
	fmt.Fprintf(w, "Started At:\t%s\n", run.StartedAt.Local().Format(time.RFC3339))
	if run.FinishedAt != nil {
		fmt.Fprintf(w, "Finished At:\t%s (Duration: %v)\n",
			run.FinishedAt.Local().Format(time.RFC3339),
			run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond))
	}
	if run.LastError != "" {
		fmt.Fprintf(w, "Last Error:\t%s\n", run.LastError)
	}
	w.Flush()
	fmt.Println("=============================================")
}

func handleStatus(ctx context.Context, st *store.Store, runID string) {
	var run *model.UpdateRun
	var err error
	if runID != "" {
		run, err = st.GetRun(ctx, runID)
	} else {
		run, err = st.GetLatestRun(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Run not found: %v\n", err)
		os.Exit(1)
	}
	printRunSummary(run)
}

func handleReport(ctx context.Context, st *store.Store, runID string) {
	var run *model.UpdateRun
	var err error
	if runID != "" {
		run, err = st.GetRun(ctx, runID)
	} else {
		run, err = st.GetLatestRun(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Run not found: %v\n", err)
		os.Exit(1)
	}

	printRunSummary(run)

	// List changes in this run
	changes, err := st.ListChanges(ctx, run.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load changes: %v\n", err)
	} else {
		fmt.Printf("\n--- Changes in Run (%d) ---\n", len(changes))
		if len(changes) == 0 {
			fmt.Println("No changes recorded in this run.")
		} else {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "Supplier ID\tType\tSummary\tTimestamp")
			for _, ch := range changes {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					ch.SupplierID, ch.ChangeType, ch.Summary, ch.CreatedAt.Local().Format("15:04:05"))
			}
			w.Flush()
		}
	}

	// List problems in this run
	problems, err := st.ListProblems(ctx, model.ProblemFilter{RunID: run.ID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load problems: %v\n", err)
	} else {
		fmt.Printf("\n--- Problems in Run (%d) ---\n", len(problems))
		if len(problems) == 0 {
			fmt.Println("No problems recorded.")
		} else {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tBatch\tSupplier ID\tKind\tResolved\tMessage")
			for _, p := range problems {
				res := "No"
				if p.Resolved {
					res = "Yes"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n",
					p.ID[:8], p.BatchNo, p.SupplierID, p.Kind, res, p.Message)
			}
			w.Flush()
		}
	}
}

func handleProducts(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("products", flag.ExitOnError)
	statusFilter := fs.String("status", "", "Filter products by status: active, withdrawn, or empty for all")
	_ = fs.Parse(args)

	products, err := st.ListProducts(ctx, model.ProductFilter{Status: *statusFilter})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list products: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Catalog Products (%d):\n", len(products))
	if len(products) == 0 {
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Supplier ID\tSKU\tName\tPrice\tStatus\tVersion\tUpdated In Run")
	for _, p := range products {
		priceStr := syncer.FormatPrice(p.PriceCents, p.Currency)
		if p.Status == model.ProductStatusWithdrawn {
			priceStr = "[WITHDRAWN]"
		}
		name := p.Name
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\tv%d\t%s\n",
			p.SupplierID, p.SKU, name, priceStr, p.Status, p.Version, p.UpdatedInRun)
	}
	w.Flush()
}

func handleProblems(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("problems", flag.ExitOnError)
	runID := fs.String("run", "", "Filter problems by run ID")
	showAll := fs.Bool("all", false, "Show all problems including resolved")
	_ = fs.Parse(args)

	filter := model.ProblemFilter{
		RunID:          *runID,
		UnresolvedOnly: !*showAll,
	}

	problems, err := st.ListProblems(ctx, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load problems: %v\n", err)
		os.Exit(1)
	}

	header := "Unresolved Problems"
	if *showAll {
		header = "All Problems"
	}
	fmt.Printf("%s (%d):\n", header, len(problems))
	if len(problems) == 0 {
		fmt.Println("No matching problems found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Problem ID\tRun ID\tBatch\tSupplier ID\tKind\tResolved\tMessage")
	for _, p := range problems {
		res := "No"
		if p.Resolved {
			res = "Yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			p.ID, p.RunID, p.BatchNo, p.SupplierID, p.Kind, res, p.Message)
	}
	w.Flush()
}

func handleResolve(ctx context.Context, st *store.Store, problemID string) {
	if strings.TrimSpace(problemID) == "" {
		fmt.Fprintln(os.Stderr, "Problem ID cannot be empty.")
		os.Exit(1)
	}
	if err := st.ResolveProblem(ctx, problemID); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve problem: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Problem %s marked as resolved.\n", problemID)
}

